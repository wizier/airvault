package iosbackup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"

	"github.com/wizier/airvault/internal/objectstore"
)

// cbcFile decrypts content stored as AES-256-CBC with a zero IV at any offset:
// plaintext block i is D(C[i]) xor C[i-1].
type cbcFile struct {
	stored *objectstore.File
	block  cipher.Block
	size   int64
}

// The plaintext ends where the PKCS#7 padding of the last block says: the size
// Manifest.db lists is stale for content that changed while it was backed up.
func newCBCFile(stored *objectstore.File, keys classKeys, wrappedKey []byte) (*cbcFile, error) {
	key, err := keys.unwrap(wrappedKey)
	if err != nil {
		return nil, fmt.Errorf("unwrap file key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if stored.Size() == 0 || stored.Size()%aes.BlockSize != 0 {
		return nil, fmt.Errorf("stored %d bytes are not whole AES blocks", stored.Size())
	}
	file := &cbcFile{stored: stored, block: block, size: stored.Size()}
	last := make([]byte, aes.BlockSize)
	if _, err := file.ReadAt(last, file.size-aes.BlockSize); err != nil {
		return nil, err
	}
	pad := int(last[aes.BlockSize-1])
	if pad < 1 || pad > aes.BlockSize || !bytes.Equal(last[aes.BlockSize-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("encrypted content has no valid padding")
	}
	file.size -= int64(pad)
	return file, nil
}

func (f *cbcFile) Size() int64 { return f.size }

func (f *cbcFile) Close() error { return f.stored.Close() }

func (f *cbcFile) ReadAt(p []byte, offset int64) (int, error) {
	if offset >= f.size {
		return 0, io.EOF
	}
	end := min(offset+int64(len(p)), f.size)
	first := offset / aes.BlockSize * aes.BlockSize
	last := (end + aes.BlockSize - 1) / aes.BlockSize * aes.BlockSize
	if end == f.size {
		last = f.stored.Size() // the padding too, so the stored object is verified
	}
	from := max(first-aes.BlockSize, 0)
	buffer := make([]byte, last-from)
	if _, err := f.stored.ReadAt(buffer, from); err != nil {
		return 0, err
	}
	iv, blocks := make([]byte, aes.BlockSize), buffer
	if first > 0 {
		iv, blocks = buffer[:aes.BlockSize], buffer[aes.BlockSize:]
	}
	cipher.NewCBCDecrypter(f.block, iv).CryptBlocks(blocks, blocks)
	n := copy(p, blocks[offset-first:end-first])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
