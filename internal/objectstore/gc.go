package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Why a snapshot cannot be restored. A scan judges it from the pool alone and
// never removes anything for it: the snapshot is whole again once the pool
// holds what it needs.
const (
	// DamageFilesMissing: the pool lacks the objects of some of its files —
	// lost, or set aside for not reading as what their names say.
	DamageFilesMissing = "files_missing"
	// DamageManifestUnreadable: the manifest itself is provably corrupt.
	DamageManifestUnreadable = "manifest_unreadable"
)

// SnapshotHealth is a snapshot's standing in a scan; Damage is "" when every
// file it lists can be read from the pool.
type SnapshotHealth struct {
	ID           string
	Damage       string
	DamagedFiles int
}

// Scan is one reading of a source: the health of each snapshot, the objects
// they reach, and the pool as it was listed.
type Scan struct {
	liveSet
	Snapshots []SnapshotHealth
	source    string
	pool      poolListing
	// Every object an unreadable manifest names: which of them it references is
	// unknown, so all are kept.
	mentioned map[string]struct{}
}

// Scan reads the source's manifests against its pool.
func (s *Store) Scan(ctx context.Context, source string) (*Scan, error) {
	return s.scan(ctx, source, nil)
}

// scan reads the source as if the snapshots in skip were already gone.
func (s *Store) scan(ctx context.Context, source string, skip map[string]struct{}) (*Scan, error) {
	objectsRoot, err := s.sourcePath(source, "objects")
	if err != nil {
		return nil, err
	}
	listed, err := listPool(objectsRoot)
	if err != nil {
		return nil, err
	}
	files, err := s.ListSnapshots(source)
	if err != nil {
		return nil, err
	}
	scan := &Scan{source: source, pool: listed, mentioned: map[string]struct{}{}}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, skipped := skip[file.ID]; skipped {
			continue
		}
		health := SnapshotHealth{ID: file.ID}
		snapshot, err := s.OpenSnapshot(source, file.ID)
		switch {
		case errors.Is(err, ErrManifestCorrupt):
			health.Damage = DamageManifestUnreadable
			if err := s.mentionObjects(source, file.ID, scan.mentioned); err != nil {
				return nil, err
			}
			if scan.manifestBytes, err = addChecked(scan.manifestBytes, file.size); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, fmt.Errorf("load published snapshot %q: %w", file.ID, err)
		default:
			if health.DamagedFiles = listed.lacking(snapshot); health.DamagedFiles > 0 {
				health.Damage = DamageFilesMissing
			}
			if err := scan.addSnapshot(snapshot, file.size); err != nil {
				return nil, err
			}
		}
		scan.Snapshots = append(scan.Snapshots, health)
	}
	return scan, nil
}

var objectRefPattern = regexp.MustCompile(`[0-9a-f]{64}`)

func (s *Store) mentionObjects(source, snapshotID string, mentioned map[string]struct{}) error {
	manifestPath, err := s.resolveManifest(snapshotManifestRelative(source, snapshotID))
	if err != nil {
		return err
	}
	file, err := os.Open(manifestPath)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxManifestSize))
	if err != nil {
		return err
	}
	for _, objectRef := range objectRefPattern.FindAll(data, -1) {
		mentioned[string(objectRef)] = struct{}{}
	}
	return nil
}

func (scan *Scan) keeps(objectRef string) bool {
	_, live := scan.objects[objectRef]
	_, mentioned := scan.mentioned[objectRef]
	return live || mentioned
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

// Sweep deletes every object the scan listed that no snapshot needs, and every
// damaged copy nothing needs or a whole copy has replaced.
func (s *Store) Sweep(scan *Scan) error {
	if err := s.ensureCollectable(scan.source); err != nil {
		return err
	}
	objectsRoot, err := s.sourcePath(scan.source, "objects")
	if err != nil {
		return err
	}
	var garbage []string
	for objectRef := range scan.pool.healthy {
		if !scan.keeps(objectRef) {
			garbage = append(garbage, pooledPath(objectsRoot, objectRef))
		}
	}
	for objectRef := range scan.pool.damaged {
		if _, replaced := scan.pool.healthy[objectRef]; replaced || !scan.keeps(objectRef) {
			garbage = append(garbage, pooledPath(objectsRoot, objectRef)+damagedSuffix)
		}
	}
	if err := s.sweepObjects(garbage); err != nil {
		return err
	}
	if scan.empty() {
		return s.removeEmptySource(scan.source)
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
	if err := s.syncFilesystem(); err != nil {
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

// ReclaimableBytes is what deleting snapshotIDs would free: their objects that
// the same scan Sweep relies on would no longer keep. An unreadable one adds
// nothing, so the answer is then a floor.
func (s *Store) ReclaimableBytes(ctx context.Context, source string, snapshotIDs []string) (int64, error) {
	skip := make(map[string]struct{}, len(snapshotIDs))
	for _, snapshotID := range snapshotIDs {
		skip[snapshotID] = struct{}{}
	}
	survivors, err := s.scan(ctx, source, skip)
	if err != nil {
		return 0, err
	}
	targets := &liveSet{}
	for _, snapshotID := range snapshotIDs {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		target, err := s.OpenSnapshot(source, snapshotID)
		if errors.Is(err, ErrManifestCorrupt) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if err := targets.addObjects(target); err != nil {
			return 0, err
		}
	}
	var total int64
	for objectRef, size := range targets.objects {
		if !survivors.keeps(objectRef) {
			if total, err = addChecked(total, size); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

type liveSet struct {
	objects       map[string]int64
	objectBytes   int64
	manifestBytes int64
}

func (live *liveSet) empty() bool {
	return live.manifestBytes == 0 && len(live.objects) == 0
}

func (live *liveSet) Footprint() int64 {
	return live.objectBytes + live.manifestBytes
}

func (live *liveSet) addSnapshot(snapshot *Snapshot, manifestSize int64) error {
	if err := live.addObjects(snapshot); err != nil {
		return err
	}
	var err error
	live.manifestBytes, err = addChecked(live.manifestBytes, manifestSize)
	return err
}

func (live *liveSet) addObjects(snapshot *Snapshot) error {
	if live.objects == nil {
		live.objects = make(map[string]int64, len(snapshot.manifest.Entries))
	}
	for _, entry := range snapshot.manifest.Entries {
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

func addChecked(total, size int64) (int64, error) {
	if size > math.MaxInt64-total {
		return 0, errors.New("object store byte total overflows int64")
	}
	return total + size, nil
}

// poolListing is a source's objects as listed: those under their names, and
// those set aside as damaged.
type poolListing struct{ healthy, damaged map[string]struct{} }

// lacking counts the files of snapshot whose objects are not in the pool.
func (listed poolListing) lacking(snapshot *Snapshot) int {
	lacking := 0
	for _, entry := range snapshot.manifest.Entries {
		if _, exists := listed.healthy[entry.ObjectRef]; !exists && entry.Kind == entryFile {
			lacking++
		}
	}
	return lacking
}

func pooledPath(objectsRoot, objectRef string) string {
	return filepath.Join(objectsRoot, objectRef[:objectPrefixLength], objectRef)
}

// Only names the store writes count; anything else is skipped and never
// removed.
func listPool(objectsRoot string) (poolListing, error) {
	listed := poolListing{healthy: map[string]struct{}{}, damaged: map[string]struct{}{}}
	prefixes, err := readDirIfExists(objectsRoot)
	if err != nil {
		return listed, err
	}
	for _, prefix := range prefixes {
		if !validLowerHex(prefix.Name(), objectPrefixLength) {
			continue // not the store's
		}
		if !prefix.IsDir() {
			return listed, fmt.Errorf("object prefix %q is not a directory", prefix.Name())
		}
		entries, err := os.ReadDir(filepath.Join(objectsRoot, prefix.Name()))
		if err != nil {
			return listed, err
		}
		for _, entry := range entries {
			objectRef, damaged := strings.CutSuffix(entry.Name(), damagedSuffix)
			if validateObjectRef(objectRef) != nil || objectRef[:objectPrefixLength] != prefix.Name() {
				continue // not the store's
			}
			// The type comes with the listing; a stat per object would dominate the
			// pass on a NAS. Content is judged where objects are read.
			if !entry.Type().IsRegular() {
				return listed, fmt.Errorf("object %q is not a regular file", filepath.Join(prefix.Name(), entry.Name()))
			}
			if damaged {
				listed.damaged[objectRef] = struct{}{}
			} else {
				listed.healthy[objectRef] = struct{}{}
			}
		}
	}
	return listed, nil
}
