//go:build !darwin && !linux

package objectstore

import (
	"errors"
	"fmt"
	"os"
)

func acquireStoreLock(root string) (*os.File, error) {
	return nil, fmt.Errorf("exclusive object-store ownership is unsupported on this platform: %s", root)
}

func releaseStoreLock(file *os.File) error { return file.Close() }

func (s *Store) Sync() error {
	return errors.New("durable object-store sync is unsupported on this platform")
}
