package objectstore

import (
	"cmp"

	"golang.org/x/sys/unix"
)

// Sync flushes the whole filesystem, making prior writes and removals durable.
// sync(2) cannot fail on Linux, so unix.Sync has no return value here.
func (s *Store) Sync() error {
	unix.Sync()
	return nil
}

// FreeSpace is what an unprivileged writer may still use on the store's volume.
func (s *Store) FreeSpace() (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(s.root, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(cmp.Or(stat.Frsize, stat.Bsize)), nil
}
