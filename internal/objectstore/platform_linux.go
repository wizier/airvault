package objectstore

import (
	"cmp"

	"golang.org/x/sys/unix"
)

// syncFilesystem flushes the whole filesystem, making prior writes and
// removals durable. sync(2) cannot fail on Linux, so unix.Sync returns nothing.
func (s *Store) syncFilesystem() error {
	unix.Sync()
	return nil
}

// freeSpace is what an unprivileged writer may still use on the store's volume.
func (s *Store) freeSpace() (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(s.root, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(cmp.Or(stat.Frsize, stat.Bsize)), nil
}
