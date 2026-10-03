package iosbackup

import (
	"bytes"
	"crypto/aes"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// The BackupKeyBag in Manifest.plist wraps its class keys with a key derived
// from the backup password. Unwrapping them proves the password locally and
// yields the keys that open Manifest.db and every file.

// ErrWrongPassword is a password that does not unwrap the keybag.
var ErrWrongPassword = errors.New("backup password is wrong")

// WRAP flag bit: this class key's WPKY is wrapped with the password key.
const wrapWithPassword = 2

type keybagClassKey struct {
	class uint32
	wrap  uint32
	wpky  []byte
}

type backupKeybag struct {
	dpsl, salt []byte
	dpic, iter uint32
	classKeys  []keybagClassKey
}

// parseKeybag walks the keybag's TLV blocks: header derivation parameters
// first, then per-class blocks each introduced by a CLAS tag.
func parseKeybag(data []byte) (*backupKeybag, error) {
	bag := &backupKeybag{}
	var current *keybagClassKey
	uint32Field := func(tag string, payload []byte) (uint32, error) {
		if len(payload) != 4 {
			return 0, fmt.Errorf("keybag block %q is not a uint32", tag)
		}
		return binary.BigEndian.Uint32(payload), nil
	}
	for offset := 0; offset < len(data); {
		if offset+8 > len(data) {
			return nil, errors.New("truncated keybag block header")
		}
		tag := string(data[offset : offset+4])
		length := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if length < 0 || offset+length > len(data) {
			return nil, fmt.Errorf("keybag block %q overruns the data", tag)
		}
		payload := data[offset : offset+length]
		offset += length
		var err error
		switch tag {
		case "DPSL":
			bag.dpsl = bytes.Clone(payload)
		case "DPIC":
			bag.dpic, err = uint32Field(tag, payload)
		case "SALT":
			bag.salt = bytes.Clone(payload)
		case "ITER":
			bag.iter, err = uint32Field(tag, payload)
		case "CLAS":
			bag.classKeys = append(bag.classKeys, keybagClassKey{})
			current = &bag.classKeys[len(bag.classKeys)-1]
			current.class, err = uint32Field(tag, payload)
		case "WRAP":
			if current != nil {
				current.wrap, err = uint32Field(tag, payload)
			}
		case "WPKY":
			if current != nil {
				current.wpky = bytes.Clone(payload)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return bag, nil
}

// classKeys are a keybag's unwrapped keys by protection class.
type classKeys map[uint32][]byte

// unlockKeybag unwraps every password-wrapped class key. The PBKDF2 stretch
// costs seconds of CPU by design (DPIC is ~10M on modern iOS).
func unlockKeybag(keybagData []byte, password string) (classKeys, error) {
	bag, err := parseKeybag(keybagData)
	if err != nil {
		return nil, err
	}
	if len(bag.salt) == 0 || bag.iter == 0 {
		return nil, errors.New("keybag has no password derivation parameters")
	}
	secret := password
	// iOS 10.2+ prepends a SHA-256 stretch (DPSL/DPIC) to the SHA-1 pass.
	if len(bag.dpsl) > 0 && bag.dpic > 0 {
		stretched, err := pbkdf2.Key(sha256.New, password, bag.dpsl, int(bag.dpic), 32)
		if err != nil {
			return nil, err
		}
		secret = string(stretched)
	}
	kek, err := pbkdf2.Key(sha1.New, secret, bag.salt, int(bag.iter), 32)
	if err != nil {
		return nil, err
	}
	keys := classKeys{}
	for _, classKey := range bag.classKeys {
		if classKey.wrap&wrapWithPassword == 0 || len(classKey.wpky) == 0 {
			continue
		}
		key, err := aesUnwrap(kek, classKey.wpky)
		if err != nil {
			return nil, ErrWrongPassword
		}
		keys[classKey.class] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("keybag has no password-wrapped class key")
	}
	return keys, nil
}

// unwrap opens a manifest or file key: a little-endian protection class, then
// the key wrapped with that class's key.
func (keys classKeys) unwrap(wrapped []byte) ([]byte, error) {
	if len(wrapped) < 4 {
		return nil, errors.New("wrapped key is too short")
	}
	class := binary.LittleEndian.Uint32(wrapped[:4])
	classKey, ok := keys[class]
	if !ok {
		return nil, fmt.Errorf("keybag has no key for protection class %d", class)
	}
	return aesUnwrap(classKey, wrapped[4:])
}

// VerifyPassword checks password against the backup's keybag locally, without
// asking the device (which would reject it ~10s into a restore exchange).
func (b *Backup) VerifyPassword(password string) (bool, error) {
	_, err := b.unlockKeys(password)
	if errors.Is(err, ErrWrongPassword) {
		return false, nil
	}
	return err == nil, err
}

func (b *Backup) unlockKeys(password string) (classKeys, error) {
	if len(b.keybag) == 0 {
		return nil, errors.New("backup keybag is missing from Manifest.plist")
	}
	return unlockKeybag(b.keybag, password)
}

// aesUnwrap is RFC 3394. A failed integrity check means the wrapping key, and
// so the password behind it, is wrong.
func aesUnwrap(kek, wrapped []byte) ([]byte, error) {
	if len(wrapped) < 24 || len(wrapped)%8 != 0 {
		return nil, errors.New("invalid wrapped key length")
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	n := len(wrapped)/8 - 1
	a := binary.BigEndian.Uint64(wrapped[:8])
	r := make([]byte, n*8)
	copy(r, wrapped[8:])
	buf := make([]byte, 16)
	for j := 5; j >= 0; j-- {
		for i := n; i >= 1; i-- {
			binary.BigEndian.PutUint64(buf[:8], a^uint64(n*j+i))
			copy(buf[8:], r[(i-1)*8:i*8])
			block.Decrypt(buf, buf)
			a = binary.BigEndian.Uint64(buf[:8])
			copy(r[(i-1)*8:i*8], buf[8:])
		}
	}
	if a != 0xA6A6A6A6A6A6A6A6 {
		return nil, errors.New("key unwrap integrity check failed")
	}
	return r, nil
}
