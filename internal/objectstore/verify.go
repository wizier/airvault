package objectstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
)

// Verify hashes every object the scan's snapshots need: one that is bad or
// cannot be read is set aside, a set-aside one that reads right again is put
// back. It returns how many it set aside, failing once all are judged if any
// could not be read; progress gets the bytes read of the total.
func (s *Store) Verify(ctx context.Context, scan *Scan, progress func(done, total int64)) (int, error) {
	objectsRoot, err := s.sourcePath(scan.source, "objects")
	if err != nil {
		return 0, err
	}
	setAsideCount, unreadable := 0, 0
	var firstUnreadable error
	var done int64
	// Sorted, the reads walk the pool directory by directory.
	for _, objectRef := range slices.Sorted(maps.Keys(scan.objects)) {
		if err := ctx.Err(); err != nil {
			return setAsideCount, err
		}
		size := scan.objects[objectRef]
		path := pooledPath(objectsRoot, objectRef)
		_, healthy := scan.pool.healthy[objectRef]
		_, damaged := scan.pool.damaged[objectRef]
		var err error
		switch {
		case healthy:
			intact, readErr := objectIntact(path, objectRef, size)
			if readErr != nil {
				unreadable++
				firstUnreadable = cmp.Or(firstUnreadable, fmt.Errorf("object %s: %w", objectRef, readErr))
			}
			if !intact {
				err = setAside(path)
				setAsideCount++
			}
		case damaged:
			// A copy that still cannot be read stays set aside.
			if intact, _ := objectIntact(path+damagedSuffix, objectRef, size); intact {
				err = putBack(path)
			}
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return setAsideCount, err
		}
		done += size
		progress(done, scan.objectBytes)
	}
	if unreadable > 0 {
		return setAsideCount, fmt.Errorf("%d objects could not be read and were set aside, the first: %w", unreadable, firstUnreadable)
	}
	return setAsideCount, nil
}

// objectIntact reports whether the file at path holds exactly objectRef of
// size bytes. A file gone since the listing is not intact, nor is one that
// cannot be read, which is the error.
func objectIntact(path, objectRef string, size int64) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return false, nil
	}
	check := newContentCheck(objectRef, size)
	_, err = io.Copy(checkWriter{check}, file)
	if err == nil {
		err = check.feed(nil) // an empty object is judged here
	}
	if errors.Is(err, ErrIntegrity) {
		return false, nil
	}
	return err == nil, err
}
