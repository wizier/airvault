// Package durable replaces files so that a crash leaves the old content or
// the new, never a mix of the two.
package durable

import (
	"crypto/rand"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile replaces path with data: the data reaches the disk before the
// rename, and the rename before WriteFile returns.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.OpenFile(filepath.Join(dir, "."+filepath.Base(path)+".tmp-"+rand.Text()),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(temp.Name()) // a no-op once renamed
	_, err = temp.Write(data)
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return SyncDir(dir)
}

// SyncDir makes the entries of dir durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
