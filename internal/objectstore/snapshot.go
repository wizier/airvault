package objectstore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/wizier/airvault/internal/domain"
)

func snapshotManifestRelative(source, snapshotID string) string {
	return path.Join(source, "snapshots", snapshotID+".json")
}

func stagingManifestRelative(source, snapshotID string) string {
	return path.Join(source, "staging", snapshotID, "manifest.json")
}

func validateSnapshotIdentity(source, snapshotID string) error {
	if err := domain.ValidateSource(source); err != nil {
		return err
	}
	return validateSnapshotID(snapshotID)
}

func validateSnapshotID(snapshotID string) error {
	parsed, err := uuid.Parse(snapshotID)
	if err != nil || parsed.String() != snapshotID {
		return fmt.Errorf("snapshot id must be a canonical UUID")
	}
	return nil
}

type Snapshot struct {
	store    *Store
	relative string
	manifest *manifest

	treeOnce sync.Once
	tree     *tree // built by the first List
}

// A sealed but unpublished snapshot. Only Publish accepts it, so the type rules
// out publishing a manifest twice.
type StagedSnapshot struct{ Snapshot }

func (snap *Snapshot) Source() string { return snap.manifest.SourceUDID }

func (snap *Snapshot) ID() string { return snap.manifest.SnapshotID }

func (snap *Snapshot) SizeBytes() int64 { return snap.manifest.SizeBytes }

// CreatedUnix is when the immutable snapshot was created: the moment its
// contents were finalized. It rebuilds created_at after SQLite is lost.
func (snap *Snapshot) CreatedUnix() int64 { return snap.manifest.CreatedUnix }

// FileSize returns the size of a regular file in the snapshot.
func (snap *Snapshot) FileSize(key string) (int64, bool) {
	entry, ok := snap.manifest.Entries[key]
	if !ok || entry.Kind != entryFile {
		return 0, false
	}
	return entry.Size, true
}

// Exists reports a file or directory of the snapshot; "" is its root.
func (snap *Snapshot) Exists(key string) bool {
	_, ok := snap.manifest.Entries[key]
	return ok || key == ""
}

func (snap *Snapshot) List(key string) ([]Entry, error) {
	snap.treeOnce.Do(func() { snap.tree = treeFromEntries(snap.manifest.Entries) })
	return snap.tree.list(key)
}

// Open verifies content against its address as it is read.
func (snap *Snapshot) Open(key string) (io.ReadCloser, error) {
	entry, ok := snap.manifest.Entries[key]
	if !ok || entry.Kind != entryFile {
		return nil, fs.ErrNotExist
	}
	return snap.store.openReader(snap.Source(), key, entry.ObjectRef, entry.Size, nil)
}

func (s *Store) OpenSnapshot(source, snapshotID string) (*Snapshot, error) {
	return s.openManifest(source, snapshotID, snapshotManifestRelative(source, snapshotID))
}

func (s *Store) openManifest(source, snapshotID, relative string) (*Snapshot, error) {
	if err := validateSnapshotIdentity(source, snapshotID); err != nil {
		return nil, err
	}
	loaded, err := s.loadManifest(relative)
	if err != nil {
		return nil, err
	}
	if loaded.SourceUDID != source || loaded.SnapshotID != snapshotID {
		return nil, corrupt(fmt.Errorf("manifest %q identity mismatch", relative))
	}
	return &Snapshot{store: s, relative: relative, manifest: loaded}, nil
}

// SnapshotFile is a published snapshot as listed, by its manifest file.
type SnapshotFile struct {
	ID       string
	Modified time.Time // when the manifest was written
	size     int64     // the manifest's own bytes, part of the footprint
}

// ListSnapshots is the single definition of a source's snapshots: catalog
// rebuild, object liveness and the footprint must agree. Only names the store
// writes count; Finder, SMB or NAS droppings are skipped and never removed.
func (s *Store) ListSnapshots(source string) ([]SnapshotFile, error) {
	dir, err := s.sourcePath(source, "snapshots")
	if err != nil {
		return nil, err
	}
	entries, err := readDirIfExists(dir)
	if err != nil {
		return nil, err
	}
	var snapshots []SnapshotFile
	for _, entry := range entries {
		id, isManifest := strings.CutSuffix(entry.Name(), ".json")
		if !isManifest || validateSnapshotID(id) != nil {
			continue // not the store's
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("snapshot manifest %q is not a regular file", entry.Name())
		}
		snapshots = append(snapshots, SnapshotFile{ID: id, Modified: info.ModTime(), size: info.Size()})
	}
	return snapshots, nil
}

// Publish atomically promotes a staged snapshot into the immutable
// snapshot namespace: objects reach stable storage before the rename, and a
// failure or crash after it is completed by Recover.
func (s *Store) Publish(staging *StagedSnapshot) (*Snapshot, error) {
	source, snapshotID := staging.manifest.SourceUDID, staging.manifest.SnapshotID
	if err := s.syncContents(); err != nil {
		return nil, fmt.Errorf("sync published snapshot contents: %w", err)
	}
	stagingPath, err := s.resolveManifest(staging.relative)
	if err != nil {
		return nil, err
	}
	finalRelative := snapshotManifestRelative(source, snapshotID)
	finalPath, err := s.resolveManifest(finalRelative)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(stagingPath, finalPath); err != nil {
		return nil, fmt.Errorf("publish object manifest: %w", err)
	}
	published := &Snapshot{store: s, relative: finalRelative, manifest: staging.manifest}
	if err := s.finishPublication(published); err != nil {
		return nil, err
	}
	return published, nil
}

// finishPublication is the post-rename tail of a publication: sync the
// namespace, drop the staging envelope. Retrying resumes the same directory
// commit.
func (s *Store) finishPublication(published *Snapshot) error {
	source := published.manifest.SourceUDID
	sourceRoot, err := s.sourcePath(source)
	if err != nil {
		return err
	}
	if err := s.syncDir(filepath.Join(sourceRoot, "snapshots")); err != nil {
		return fmt.Errorf("sync published object manifest: %w", err)
	}
	if err := s.syncDir(sourceRoot); err != nil {
		return fmt.Errorf("sync published snapshot directory: %w", err)
	}
	if err := s.Discard(source, published.manifest.SnapshotID); err != nil {
		return fmt.Errorf("remove published staging directory: %w", err)
	}
	return nil
}

// Discard drops an unpublished snapshot's staging: a stranded one blocks every
// later collection for the source.
func (s *Store) Discard(source, snapshotID string) error {
	if err := validateSnapshotID(snapshotID); err != nil {
		return err
	}
	directory, err := s.sourcePath(source, "staging", snapshotID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return syncExistingDirectory(filepath.Dir(directory))
}

// Recover settles a snapshot whose run ended without Publish or Discard: a
// published one has its publication finished and is returned; otherwise its
// staging is discarded, with the error that kept a manifest from opening.
func (s *Store) Recover(source, snapshotID string) (*Snapshot, error) {
	published, unopened, err := s.settle(source, snapshotID)
	if err != nil {
		return nil, err
	}
	return published, unopened
}

// settle returns why an existing manifest did not open apart from what failed
// the settling itself.
func (s *Store) settle(source, snapshotID string) (published *Snapshot, unopened, err error) {
	published, openErr := s.OpenSnapshot(source, snapshotID)
	if openErr == nil {
		return published, nil, s.finishPublication(published)
	}
	if err := s.Discard(source, snapshotID); err != nil {
		return nil, nil, err
	}
	if errors.Is(openErr, fs.ErrNotExist) {
		return nil, nil, nil
	}
	return nil, openErr, nil
}

// RecoverSource settles every snapshot of source left in staging. Startup runs
// it per source, as does a run that dies without its own cleanup.
func (s *Store) RecoverSource(source string) error {
	stagingRoot, err := s.sourcePath(source, "staging")
	if err != nil {
		return err
	}
	entries, err := readDirIfExists(stagingRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		snapshotID := entry.Name()
		if validateSnapshotID(snapshotID) != nil {
			continue // not the store's
		}
		// A manifest that does not open is the catalog's to report.
		if _, _, err := s.settle(source, snapshotID); err != nil {
			return err
		}
	}
	if err := removeDirIfEmpty(stagingRoot); err != nil {
		return err
	}
	return syncExistingDirectory(filepath.Dir(stagingRoot))
}

func (s *Store) RemoveSnapshot(source, snapshotID string) error {
	if err := validateSnapshotIdentity(source, snapshotID); err != nil {
		return err
	}
	manifestPath, err := s.resolveManifest(snapshotManifestRelative(source, snapshotID))
	if err != nil {
		return err
	}
	info, err := os.Lstat(manifestPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return syncExistingDirectory(filepath.Dir(manifestPath))
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return fmt.Errorf("published snapshot manifest is not a regular file: %s", manifestPath)
	}
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove snapshot manifest: %w", err)
	}
	if err := syncExistingDirectory(filepath.Dir(manifestPath)); err != nil {
		return fmt.Errorf("sync snapshot manifest deletion: %w", err)
	}
	return nil
}
