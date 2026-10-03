package iosbackup

import (
	"bytes"
	"crypto/aes"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"
)

// aesWrap is the RFC 3394 forward operation, used only to build fixtures.
func aesWrap(t *testing.T, kek, plain []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(kek)
	if err != nil {
		t.Fatal(err)
	}
	n := len(plain) / 8
	a := uint64(0xA6A6A6A6A6A6A6A6)
	r := append([]byte(nil), plain...)
	buf := make([]byte, 16)
	for j := 0; j <= 5; j++ {
		for i := 1; i <= n; i++ {
			binary.BigEndian.PutUint64(buf[:8], a)
			copy(buf[8:], r[(i-1)*8:i*8])
			block.Encrypt(buf, buf)
			a = binary.BigEndian.Uint64(buf[:8]) ^ uint64(n*j+i)
			copy(r[(i-1)*8:i*8], buf[8:])
		}
	}
	out := make([]byte, 8+len(r))
	binary.BigEndian.PutUint64(out[:8], a)
	copy(out[8:], r)
	return out
}

func tlv(tag string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	copy(out, tag)
	binary.BigEndian.PutUint32(out[4:8], uint32(len(payload)))
	copy(out[8:], payload)
	return out
}

func u32(value uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, value)
	return out
}

// buildKeybag wraps one class key for the password using the same derivation
// unlockKeybag performs; tiny iteration counts keep the test fast.
func buildKeybag(t *testing.T, password string, modern bool) []byte {
	t.Helper()
	dpsl := bytes.Repeat([]byte{0xd5}, 20)
	salt := bytes.Repeat([]byte{0x5a}, 20)
	const dpic, iter = 37, 11

	secret := password
	if modern {
		stretched, err := pbkdf2.Key(sha256.New, password, dpsl, dpic, 32)
		if err != nil {
			t.Fatal(err)
		}
		secret = string(stretched)
	}
	kek, err := pbkdf2.Key(sha1.New, secret, salt, iter, 32)
	if err != nil {
		t.Fatal(err)
	}
	classKey := bytes.Repeat([]byte{0xc1}, 32)

	bag := tlv("VERS", u32(4))
	if modern {
		bag = append(bag, tlv("DPIC", u32(dpic))...)
		bag = append(bag, tlv("DPSL", dpsl)...)
	}
	bag = append(bag, tlv("SALT", salt)...)
	bag = append(bag, tlv("ITER", u32(iter))...)
	// A device-wrapped class key first: it must be skipped, not tried.
	bag = append(bag, tlv("CLAS", u32(1))...)
	bag = append(bag, tlv("WRAP", u32(1))...)
	bag = append(bag, tlv("WPKY", bytes.Repeat([]byte{0xee}, 40))...)
	bag = append(bag, tlv("CLAS", u32(2))...)
	bag = append(bag, tlv("WRAP", u32(3))...)
	bag = append(bag, tlv("WPKY", aesWrap(t, kek, classKey))...)
	return bag
}

// RFC 3394 §4.6 vector (256-bit KEK, 256-bit key data): guards against a
// shared implementation error that the wrap/unwrap roundtrip could not see.
func TestAESUnwrapRFC3394Vector(t *testing.T) {
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i)
	}
	plain, _ := hex.DecodeString("00112233445566778899AABBCCDDEEFF000102030405060708090A0B0C0D0E0F")
	wrapped, _ := hex.DecodeString(
		"28C9F404C4B810F4CBCCB35CFB87F8263F5786E2D80ED326CBC7F0E71A99F43BFB988B9B7A02DD21")
	got, err := aesUnwrap(kek, wrapped)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("aesUnwrap(RFC 3394 4.6) = %x, err=%v; want %x", got, err, plain)
	}
	if !bytes.Equal(aesWrap(t, kek, plain), wrapped) {
		t.Fatal("aesWrap fixture builder does not reproduce the RFC 3394 4.6 vector")
	}
}

func TestUnlockKeybag(t *testing.T) {
	for _, modern := range []bool{true, false} {
		bag := buildKeybag(t, "correct horse", modern)
		keys, err := unlockKeybag(bag, "correct horse")
		if err != nil || !bytes.Equal(keys[2], bytes.Repeat([]byte{0xc1}, 32)) || len(keys) != 1 {
			t.Fatalf("modern=%v correct password: keys=%x err=%v", modern, keys, err)
		}
		if _, err := unlockKeybag(bag, "battery staple"); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("modern=%v wrong password: err=%v, want ErrWrongPassword", modern, err)
		}
	}
}

func TestUnlockKeybagRejectsUnusableKeybags(t *testing.T) {
	noParams := append(tlv("VERS", u32(4)), tlv("CLAS", u32(2))...)
	// Only device-wrapped class keys: nothing can prove the password.
	onlyDevice := append(tlv("SALT", bytes.Repeat([]byte{1}, 20)), tlv("ITER", u32(5))...)
	onlyDevice = append(onlyDevice, tlv("CLAS", u32(1))...)
	onlyDevice = append(onlyDevice, tlv("WRAP", u32(1))...)
	onlyDevice = append(onlyDevice, tlv("WPKY", bytes.Repeat([]byte{2}, 40))...)
	for name, bag := range map[string][]byte{"garbage": []byte("garbage!"), "no parameters": noParams, "device keys only": onlyDevice} {
		if _, err := unlockKeybag(bag, "pw"); err == nil || errors.Is(err, ErrWrongPassword) {
			t.Fatalf("%s keybag: err=%v, want an error other than ErrWrongPassword", name, err)
		}
	}
}
