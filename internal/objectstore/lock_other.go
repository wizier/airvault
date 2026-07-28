//go:build !darwin && !linux

package objectstore

import (
	"fmt"
	"os"
)

func acquireStoreLock(root string) (*os.File, error) {
	return nil, fmt.Errorf("exclusive object-store ownership is unsupported on this platform: %s", root)
}

func releaseStoreLock(file *os.File) error { return file.Close() }
