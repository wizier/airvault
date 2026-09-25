package objectstore

import "golang.org/x/sys/unix"

// Sync flushes the whole filesystem, making prior writes and removals durable.
func (s *Store) Sync() error {
	return unix.Sync()
}
