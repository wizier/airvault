package objectstore

import "golang.org/x/sys/unix"

// Sync flushes the whole filesystem, making prior writes and removals durable.
// sync(2) cannot fail on Linux, so unix.Sync has no return value here.
func (s *Store) Sync() error {
	unix.Sync()
	return nil
}
