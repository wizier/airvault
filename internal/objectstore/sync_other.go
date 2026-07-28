//go:build !darwin && !linux

package objectstore

import "errors"

func (s *Store) Sync() error {
	return errors.New("durable object-store sync is unsupported on this platform")
}
