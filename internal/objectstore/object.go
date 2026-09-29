package objectstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
)

// openObject opens a pooled object, checking it is a regular file of size bytes.
func (s *Store) openObject(source, ref string, size int64) (*os.File, error) {
	path, err := s.resolveObjectRef(source, ref)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	switch {
	case err != nil:
	case !info.Mode().IsRegular():
		err = fmt.Errorf("object %s is not a regular file", ref)
	case info.Size() != size:
		err = errors.Join(fmt.Errorf("object %s is %d bytes, the manifest says %d", ref, info.Size(), size),
			setAside(path))
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// setAside takes an object that proved not to be what its name says out of
// the pool: the next scan marks the snapshots needing it damaged, and Verify
// puts it back should it read right again.
func setAside(path string) error {
	if err := os.Rename(path, path+damagedSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("set damaged object aside: %w", err)
	}
	return nil
}

// contentCheck hashes an object's bytes, fed in order from its start, and
// checks them against the object's address once all have arrived.
type contentCheck struct {
	ref       string
	size, fed int64
	digest    hash.Hash
	done      bool
}

func newContentCheck(ref string, size int64) *contentCheck {
	return &contentCheck{ref: ref, size: size, digest: sha256.New()}
}

func (c *contentCheck) feed(p []byte) error {
	if c.done {
		return nil
	}
	c.digest.Write(p)
	c.fed += int64(len(p))
	if c.fed < c.size {
		return nil
	}
	c.done = true
	if hex.EncodeToString(c.digest.Sum(nil)) != c.ref {
		return fmt.Errorf("%w: object %s does not hash to its name", ErrIntegrity, c.ref)
	}
	return nil
}

// checkWriter feeds what is written to it into a content check.
type checkWriter struct{ check *contentCheck }

func (w checkWriter) Write(p []byte) (int, error) { return len(p), w.check.feed(p) }

// objectReader reads exactly an object's size and fails with ErrIntegrity
// when the content is short or does not hash to its address.
type objectReader struct {
	file      *os.File
	key       string
	remaining int64
	check     *contentCheck
	fail      func(error) error // records the failure; nil when no one does
	err       error
}

// openReader opens the object at key for a verified read.
func (s *Store) openReader(source, key, ref string, size int64, fail func(error) error) (io.ReadCloser, error) {
	file, err := s.openObject(source, ref, size)
	if err != nil {
		err = fmt.Errorf("%w: open %q: %v", ErrIntegrity, key, err)
		if fail != nil {
			err = fail(err)
		}
		return nil, err
	}
	return &objectReader{file: file, key: key, remaining: size, check: newContentCheck(ref, size), fail: fail}, nil
}

func (r *objectReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.remaining == 0 {
		if err := r.check.feed(nil); err != nil {
			return 0, r.failed(fmt.Errorf("%q: %w", r.key, errors.Join(err, setAside(r.file.Name()))))
		}
		return 0, io.EOF
	}
	n, err := r.file.Read(p[:min(int64(len(p)), r.remaining)])
	r.remaining -= int64(n)
	checkErr := r.check.feed(p[:n])
	switch {
	case err != nil && err != io.EOF:
		return n, r.failed(fmt.Errorf("read %q: %w", r.key, err))
	case n == 0 && err == io.EOF:
		return 0, r.failed(fmt.Errorf("%w: %q ended before its manifest size", ErrIntegrity, r.key))
	case checkErr != nil:
		return n, r.failed(fmt.Errorf("%q: %w", r.key, errors.Join(checkErr, setAside(r.file.Name()))))
	}
	return n, nil
}

func (r *objectReader) failed(err error) error {
	if r.fail != nil {
		err = r.fail(err)
	}
	r.err = err
	return err
}

func (r *objectReader) Close() error { return r.file.Close() }
