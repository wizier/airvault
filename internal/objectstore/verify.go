package objectstore

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
)

// Verify hashes every object the scan's snapshots need: a bad one is set
// aside, a set-aside one that reads right again is put back. It returns how
// many it set aside; progress gets the bytes read of the total.
func (s *Store) Verify(ctx context.Context, scan *Scan, progress func(done, total int64)) (int, error) {
	objectsRoot, err := s.sourcePath(scan.source, "objects")
	if err != nil {
		return 0, err
	}
	setAsideCount := 0
	var done int64
	// Sorted, the reads walk the pool directory by directory.
	for _, objectRef := range slices.Sorted(maps.Keys(scan.objects)) {
		if err := ctx.Err(); err != nil {
			return setAsideCount, err
		}
		size := scan.objects[objectRef]
		path := pooledPath(objectsRoot, objectRef)
		var intact bool
		var err error
		if _, healthy := scan.pool.healthy[objectRef]; healthy {
			if intact, err = objectIntact(path, objectRef, size); err == nil && !intact {
				err = setAside(path)
				setAsideCount++
			}
		} else if _, damaged := scan.pool.damaged[objectRef]; damaged {
			if intact, err = objectIntact(path+damagedSuffix, objectRef, size); err == nil && intact {
				err = putBack(path)
			}
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return setAsideCount, err
		}
		done += size
		progress(done, scan.objectBytes)
	}
	return setAsideCount, nil
}

// objectIntact reports whether the file at path holds exactly objectRef of
// size bytes. A file gone since the listing is not intact.
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
