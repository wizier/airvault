package objectstore

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"github.com/wizier/airvault/internal/durable"
)

func readDirIfExists(directory string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

// removeDirIfEmpty treats leftover content as success — hidden junk from SMB
// clients may legitimately keep a directory alive.
func removeDirIfEmpty(directory string) error {
	err := os.Remove(directory)
	if err == nil || errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return nil
	}
	return err
}

func syncExistingDirectory(directory string) error {
	err := durable.SyncDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
