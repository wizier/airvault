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
	"sort"
	"syscall"
)

// LiveSet is a source's reachable objects plus the byte totals accumulated while
// reading the manifests that reach them.
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
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !hiddenEntry(entry.Name()) {
			return fmt.Errorf("collect source %q: staging snapshot exists", source)
		}
	}
	return nil
}

// CollectLive deletes every object the live set does not reach and warns about
// damaged live ones — a missing or wrong-size object never wedges the pass and is
// never deleted itself. What survives is the live set, so its Footprint is final.
func (s *Store) CollectLive(source string, live *LiveSet) error {
	if live == nil {
		return errors.New("live object set is nil")
	}
	if err := s.ensureCollectable(source); err != nil {
		return err
	}
	objectsRoot, err := s.sourcePath(source, "objects")
	if err != nil {
		return err
	}
	paths, seen, err := s.collectableObjects(objectsRoot, live)
	if err != nil {
		return err
	}
	for objectRef := range live.objects {
		if _, exists := seen[objectRef]; !exists {
			slog.Warn("collect: live object missing", "source", source, "object", objectRef)
		}
	}
	if err := s.sweepObjects(objectsRoot, paths); err != nil {
		return err
	}
	if live.empty() {
		return s.removeEmptySource(source)
	}
	return nil
}

// sweepObjects unlinks the garbage, then the prefixes it emptied.
func (s *Store) sweepObjects(objectsRoot string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		if err := os.Remove(filePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, filePath := range paths {
		if err := removeEmptyObjectPrefix(filepath.Dir(filePath), objectsRoot); err != nil {
			return err
		}
	}
	if err := s.Sync(); err != nil {
		return fmt.Errorf("sync collected object storage: %w", err)
	}
	return nil
}

// removeEmptySource drops the directories of a source with nothing left to
// reach. Leftover content keeps the tree: nothing unrecognised is removed here.
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

// empty reports a source that reaches nothing: no valid manifest, no object.
func (live *LiveSet) empty() bool {
	return live.manifestBytes == 0 && len(live.objects) == 0
}

// Footprint is the reachable object payload plus the manifests that reach it.
// Both totals are already accumulated, so this traverses nothing.
func (live *LiveSet) Footprint() (int64, error) {
	return addChecked(live.objectBytes, live.manifestBytes)
}

// addChecked keeps the store's byte arithmetic from wrapping.
func addChecked(total, size int64) (int64, error) {
	if size > math.MaxInt64-total {
		return 0, errors.New("object store byte total overflows int64")
	}
	return total + size, nil
}

// SnapshotManifestBytes is the on-disk size of one published manifest.
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

func (s *Store) collectableObjects(
	objectsRoot string,
	live *LiveSet,
) ([]string, map[string]struct{}, error) {
	prefixes, err := os.ReadDir(objectsRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	garbage := make([]string, 0)
	seen := make(map[string]struct{}, len(live.objects))
	for _, prefix := range prefixes {
		if hiddenEntry(prefix.Name()) {
			continue
		}
		if !validLowerHex(prefix.Name(), objectPrefixLength) || !prefix.IsDir() {
			return nil, nil, fmt.Errorf("unexpected object prefix %q", prefix.Name())
		}
		entries, err := os.ReadDir(filepath.Join(objectsRoot, prefix.Name()))
		if err != nil {
			return nil, nil, err
		}
		for _, entry := range entries {
			objectRef := entry.Name()
			if hiddenEntry(objectRef) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, nil, err
			}
			if validateObjectRef(objectRef) != nil ||
				objectRef[:objectPrefixLength] != prefix.Name() ||
				!info.Mode().IsRegular() {
				return nil, nil, fmt.Errorf(
					"unexpected object entry %q",
					filepath.Join(prefix.Name(), objectRef),
				)
			}
			if expectedSize, exists := live.objects[objectRef]; exists {
				if info.Size() != expectedSize {
					slog.Warn("collect: live object damaged",
						"source", filepath.Base(filepath.Dir(objectsRoot)), "object", objectRef)
				}
				seen[objectRef] = struct{}{}
				continue
			}
			garbage = append(garbage, filepath.Join(objectsRoot, prefix.Name(), objectRef))
		}
	}
	return garbage, seen, nil
}

func removeEmptyObjectPrefix(directory, objectsRoot string) error {
	if directory == objectsRoot {
		return nil
	}
	return removeDirIfEmpty(directory)
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

// ReclaimableBytes reports how many bytes deleting the given snapshots
// together would free: objects they reference that no kept snapshot references.
func (s *Store) ReclaimableBytes(ctx context.Context, source string, snapshotIDs []string) (int64, error) {
	skip := make(map[string]struct{}, len(snapshotIDs))
	targetObjects := &LiveSet{}
	for _, snapshotID := range snapshotIDs {
		if err := validateSnapshotIdentity(source, snapshotID); err != nil {
			return 0, err
		}
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

// LiveObjects collects every object a published manifest still references.
// Snapshots in skip count as already deleted; nil keeps them all.
func (s *Store) LiveObjects(ctx context.Context, source string, skip map[string]struct{}) (*LiveSet, error) {
	manifests, err := s.listSnapshotManifests(source)
	if err != nil {
		return nil, err
	}
	live := &LiveSet{}
	for _, manifest := range manifests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, skipped := skip[manifest.id]; skipped {
			continue
		}
		view, err := s.OpenSnapshot(source, manifest.id)
		if err != nil {
			return nil, fmt.Errorf("load published snapshot %q: %w", manifest.id, err)
		}
		if err := addViewObjects(view, live); err != nil {
			return nil, err
		}
		if live.manifestBytes, err = addChecked(live.manifestBytes, manifest.size); err != nil {
			return nil, err
		}
	}
	return live, nil
}

// ScanLive builds the live set from every manifest whose seal verifies and
// returns the ids of provably corrupt ones instead of failing on them, so one
// bad manifest cannot hide the rest.
func (s *Store) ScanLive(ctx context.Context, source string) (*LiveSet, []string, error) {
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
