package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

type LiveSet struct {
	objects       map[string]int64
	objectBytes   int64
	manifestBytes int64
}

func (s *Store) ensureCollectable(source string) error {
	stagingRoot, err := s.sourcePath(source, "staging")
	if err != nil {
		return err
	}
	entries, err := readDirIfExists(stagingRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if validateSnapshotID(entry.Name()) == nil {
			return fmt.Errorf("collect source %q: staging snapshot exists", source)
		}
	}
	return nil
}

// CollectLive deletes every object the live set does not reach and warns about
// missing live ones — pool damage never wedges the pass, and a live object is
// never deleted. What survives is the live set, so its Footprint is final.
func (s *Store) CollectLive(source string, live *LiveSet) error {
	if err := s.ensureCollectable(source); err != nil {
		return err
	}
	objectsRoot, err := s.sourcePath(source, "objects")
	if err != nil {
		return err
	}
	paths, seen, err := collectableObjects(objectsRoot, live)
	if err != nil {
		return err
	}
	for objectRef := range live.objects {
		if _, exists := seen[objectRef]; !exists {
			slog.Warn("collect: live object missing", "source", source, "object", objectRef)
		}
	}
	if err := s.sweepObjects(paths); err != nil {
		return err
	}
	if live.empty() {
		return s.removeEmptySource(source)
	}
	return nil
}

func (s *Store) sweepObjects(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	for _, filePath := range paths {
		if err := os.Remove(filePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, filePath := range paths {
		if err := removeDirIfEmpty(filepath.Dir(filePath)); err != nil {
			return err
		}
	}
	if err := s.Sync(); err != nil {
		return fmt.Errorf("sync collected object storage: %w", err)
	}
	return nil
}

// Leftover content keeps the tree: nothing unrecognised is removed here.
func (s *Store) removeEmptySource(source string) error {
	sourceRoot, err := s.sourcePath(source)
	if err != nil {
		return err
	}
	for _, name := range []string{"objects", "snapshots", "staging"} {
		if err := removeDirIfEmpty(filepath.Join(sourceRoot, name)); err != nil {
			return err
		}
	}
	if err := removeDirIfEmpty(sourceRoot); err != nil {
		return err
	}
	return syncExistingDirectory(s.root)
}

func (live *LiveSet) empty() bool {
	return live.manifestBytes == 0 && len(live.objects) == 0
}

func (live *LiveSet) Footprint() int64 {
	return live.objectBytes + live.manifestBytes
}

func addChecked(total, size int64) (int64, error) {
	if size > math.MaxInt64-total {
		return 0, errors.New("object store byte total overflows int64")
	}
	return total + size, nil
}

func (s *Store) SnapshotManifestBytes(source, snapshotID string) (int64, error) {
	if err := validateSnapshotIdentity(source, snapshotID); err != nil {
		return 0, err
	}
	manifestPath, err := s.resolveManifest(snapshotManifestRelative(source, snapshotID))
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("published snapshot manifest is not a regular file: %s", manifestPath)
	}
	return info.Size(), nil
}

func collectableObjects(objectsRoot string, live *LiveSet) ([]string, map[string]struct{}, error) {
	prefixes, err := readDirIfExists(objectsRoot)
	if err != nil {
		return nil, nil, err
	}
	garbage := make([]string, 0)
	seen := make(map[string]struct{}, len(live.objects))
	for _, prefix := range prefixes {
		if !validLowerHex(prefix.Name(), objectPrefixLength) {
			continue // not the store's
		}
		if !prefix.IsDir() {
			return nil, nil, fmt.Errorf("object prefix %q is not a directory", prefix.Name())
		}
		entries, err := os.ReadDir(filepath.Join(objectsRoot, prefix.Name()))
		if err != nil {
			return nil, nil, err
		}
		for _, entry := range entries {
			objectRef := entry.Name()
			if validateObjectRef(objectRef) != nil || objectRef[:objectPrefixLength] != prefix.Name() {
				continue // not the store's
			}
			// The type comes with the listing; a stat per object would dominate the
			// pass on a NAS. Damage is caught where objects are read or rewritten.
			if !entry.Type().IsRegular() {
				return nil, nil, fmt.Errorf("object %q is not a regular file", filepath.Join(prefix.Name(), objectRef))
			}
			if _, exists := live.objects[objectRef]; exists {
				seen[objectRef] = struct{}{}
				continue
			}
			garbage = append(garbage, filepath.Join(objectsRoot, prefix.Name(), objectRef))
		}
	}
	return garbage, seen, nil
}

// removeDirIfEmpty treats leftover content as success — hidden junk from SMB
// clients may legitimately keep a directory alive.
func removeDirIfEmpty(directory string) error {
	err := os.Remove(directory)
	if err == nil || errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return nil
	}
	return err
}

func (s *Store) ReclaimableBytes(ctx context.Context, source string, snapshotIDs []string) (int64, error) {
	skip := make(map[string]struct{}, len(snapshotIDs))
	targetObjects := &LiveSet{}
	for _, snapshotID := range snapshotIDs {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		skip[snapshotID] = struct{}{}
		target, err := s.OpenSnapshot(source, snapshotID)
		if err != nil {
			return 0, err
		}
		if err := addViewObjects(target, targetObjects); err != nil {
			return 0, err
		}
	}
	keptObjects, err := s.LiveObjects(ctx, source, skip)
	if err != nil {
		return 0, err
	}
	var total int64
	for objectRef, size := range targetObjects.objects {
		if _, kept := keptObjects.objects[objectRef]; !kept {
			if total, err = addChecked(total, size); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

// Unlike ScanLive, LiveObjects fails on a corrupt manifest. Snapshots in skip
// count as already deleted.
func (s *Store) LiveObjects(ctx context.Context, source string, skip map[string]struct{}) (*LiveSet, error) {
	live, corrupt, err := s.ScanLive(ctx, source, skip)
	if err != nil {
		return nil, err
	}
	if len(corrupt) > 0 {
		return nil, fmt.Errorf("load published snapshot %q: %w", corrupt[0], ErrManifestCorrupt)
	}
	return live, nil
}

// ScanLive returns the ids of provably corrupt manifests instead of failing on
// them, so one bad manifest cannot hide the rest.
func (s *Store) ScanLive(ctx context.Context, source string, skip map[string]struct{}) (*LiveSet, []string, error) {
	manifests, err := s.listSnapshotManifests(source)
	if err != nil {
		return nil, nil, err
	}
	live := &LiveSet{}
	var corrupt []string
	for _, manifest := range manifests {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if _, skipped := skip[manifest.id]; skipped {
			continue
		}
		view, err := s.OpenSnapshot(source, manifest.id)
		if err != nil {
			if errors.Is(err, ErrManifestCorrupt) {
				corrupt = append(corrupt, manifest.id)
				continue
			}
			return nil, nil, fmt.Errorf("load published snapshot %q: %w", manifest.id, err)
		}
		if err := addViewObjects(view, live); err != nil {
			return nil, nil, err
		}
		if live.manifestBytes, err = addChecked(live.manifestBytes, manifest.size); err != nil {
			return nil, nil, err
		}
	}
	return live, corrupt, nil
}

func addViewObjects(view *View, live *LiveSet) error {
	if live.objects == nil {
		live.objects = make(map[string]int64, len(view.manifest.Entries))
	}
	for _, entry := range view.manifest.Entries {
		if entry.Kind != entryFile {
			continue
		}
		if previous, exists := live.objects[entry.ObjectRef]; exists {
			if previous != entry.Size {
				return fmt.Errorf(
					"manifests give object %q conflicting sizes",
					entry.ObjectRef,
				)
			}
			continue
		}
		var err error
		live.objectBytes, err = addChecked(live.objectBytes, entry.Size)
		if err != nil {
			return err
		}
		live.objects[entry.ObjectRef] = entry.Size
	}
	return nil
}
