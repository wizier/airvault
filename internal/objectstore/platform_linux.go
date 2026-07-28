package objectstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const storeLockName = ".airvault.lock"

func acquireStoreLock(root string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(root, storeLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open object store ownership lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("object store %q is already owned by another process", root)
		}
		return nil, fmt.Errorf("lock object store: %w", err)
	}
	return file, nil
}

func releaseStoreLock(file *os.File) error {
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return errors.Join(unlockErr, file.Close())
}

// Sync flushes the whole filesystem, making prior writes and removals durable.
// sync(2) cannot fail on Linux, so unix.Sync has no return value here.
func (s *Store) Sync() error {
	unix.Sync()
	return nil
}
