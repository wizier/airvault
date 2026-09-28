package objectstore

import "golang.org/x/sys/unix"

// Sync flushes the whole filesystem, making prior writes and removals durable.
func (s *Store) Sync() error {
	return unix.Sync()
}

// FreeSpace is what an unprivileged writer may still use on the store's volume.
func (s *Store) FreeSpace() (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(s.root, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
