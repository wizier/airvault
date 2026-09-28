package objectstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/durable"

	"github.com/google/uuid"
)

// A Store owns its root exclusively; the lock file keeps other processes out.
type Store struct {
	root     string
	lockFile *os.File
	// Publication's syncs; tests replace them to fail either side of the rename.
	syncContents func() error
	syncDir      func(string) error
}

const objectPrefixLength = 2

func New(root string) (*Store, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve object store root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return nil, fmt.Errorf("create object store root: %w", err)
	}
	lockFile, err := acquireStoreLock(absolute)
	if err != nil {
		return nil, err
	}
	store := &Store{root: absolute, lockFile: lockFile, syncDir: durable.SyncDir}
	store.syncContents = store.Sync
	return store, nil
}

func (s *Store) Close() error {
	if s.lockFile == nil {
		return nil
	}
	file := s.lockFile
	s.lockFile = nil
	return releaseStoreLock(file)
}

// Parts are not validated here: they are the store's own literals or
// already-validated ids.
func (s *Store) sourcePath(source string, parts ...string) (string, error) {
	if err := domain.ValidateSource(source); err != nil {
		return "", err
	}
	target := filepath.Join(append([]string{s.root, source}, parts...)...)
	if err := rejectSymlinkTraversal(s.root, target); err != nil {
		return "", err
	}
	return target, nil
}

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

// Publish atomically promotes a sealed staging view into the immutable
// snapshot namespace: objects reach stable storage before the rename, and a
// crash after it is completed by FinishPublication during recovery.
func (s *Store) Publish(staging *StagingView) (*View, error) {
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
	published := &View{store: s, relative: finalRelative, manifest: staging.manifest}
	if err := s.FinishPublication(published); err != nil {
		return nil, err
	}
	return published, nil
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

// Nothing in the source's tree is reachable once UnpublishSource returns.
func (s *Store) UnpublishSource(source string) error {
	sourceRoot, err := s.sourcePath(source)
	if err != nil {
		return err
	}
	for _, name := range []string{"snapshots", "staging"} {
		if err := os.RemoveAll(filepath.Join(sourceRoot, name)); err != nil {
			return fmt.Errorf("unpublish source: remove %s: %w", name, err)
		}
	}
	return syncExistingDirectory(sourceRoot)
}

// RemoveSourceTree reclaims what UnpublishSource left unreachable.
func (s *Store) RemoveSourceTree(source string) error {
	sourceRoot, err := s.sourcePath(source)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(sourceRoot); err != nil {
		return fmt.Errorf("remove source tree: %w", err)
	}
	return syncExistingDirectory(s.root)
}

// FinishPublication is the post-rename tail of a publication: sync the
// namespace, drop the staging envelope. Crash recovery calls it directly and
// retrying resumes the same directory commit.
func (s *Store) FinishPublication(published *View) error {
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
	if err := s.DiscardStaging(source, published.manifest.SnapshotID); err != nil {
		return fmt.Errorf("remove published staging directory: %w", err)
	}
	return nil
}

// A stranded staging envelope blocks every later collection for the source.
func (s *Store) DiscardStaging(source, snapshotID string) error {
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

// ReconcileSourceStaging resolves a device's filesystem transactions: published
// manifests finish their durability boundary, unpublished ones are discarded.
// Startup runs it per source, as does a run that dies without its own cleanup.
func (s *Store) ReconcileSourceStaging(source string) error {
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
		published, openErr := s.OpenSnapshot(source, snapshotID)
		if openErr == nil {
			if err := s.FinishPublication(published); err != nil {
				return err
			}
		} else if err := s.DiscardStaging(source, snapshotID); err != nil {
			return err
		}
	}
	if err := removeDirIfEmpty(stagingRoot); err != nil {
		return err
	}
	return syncExistingDirectory(filepath.Dir(stagingRoot))
}

func (s *Store) ListSources() ([]string, error) {
	entries, err := readDirIfExists(s.root)
	if err != nil {
		return nil, err
	}
	var sources []string
	for _, entry := range entries {
		if entry.IsDir() && domain.ValidateSource(entry.Name()) == nil {
			sources = append(sources, entry.Name())
		}
	}
	return sources, nil
}

func readDirIfExists(directory string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

type snapshotManifestFile struct {
	id   string
	size int64
}

// The single definition of a source's snapshots: catalog rebuild, object
// liveness and the footprint must agree. Only names the store writes count;
// Finder, SMB or NAS droppings are skipped and never removed.
func (s *Store) listSnapshotManifests(source string) ([]snapshotManifestFile, error) {
	dir, err := s.sourcePath(source, "snapshots")
	if err != nil {
		return nil, err
	}
	entries, err := readDirIfExists(dir)
	if err != nil {
		return nil, err
	}
	var manifests []snapshotManifestFile
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
		manifests = append(manifests, snapshotManifestFile{id: id, size: info.Size()})
	}
	return manifests, nil
}

func (s *Store) ListSnapshotIDs(source string) ([]string, error) {
	manifests, err := s.listSnapshotManifests(source)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, manifest := range manifests {
		ids = append(ids, manifest.id)
	}
	return ids, nil
}

func (s *Store) resolveManifest(relative string) (string, error) {
	target := filepath.Join(s.root, filepath.FromSlash(relative))
	if err := rejectSymlinkTraversal(s.root, target); err != nil {
		return "", err
	}
	return target, nil
}

func (s *Store) resolveObjectRef(source, objectRef string) (string, error) {
	if err := validateObjectRef(objectRef); err != nil {
		return "", err
	}
	return s.sourcePath(source, "objects", objectRef[:objectPrefixLength], objectRef)
}

func validateObjectRef(objectRef string) error {
	if !validSHA256(objectRef) {
		return fmt.Errorf("invalid object reference %q", objectRef)
	}
	return nil
}

func rejectSymlinkTraversal(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || !filepath.IsLocal(relative) {
		return fmt.Errorf("storage path escaped backup root")
	}
	current := root
	for component := range strings.SplitSeq(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		switch {
		case statErr == nil && info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("storage path crosses symlink %q", current)
		case statErr == nil:
		case errors.Is(statErr, fs.ErrNotExist):
			return nil
		default:
			return statErr
		}
	}
	return nil
}

func syncExistingDirectory(directory string) error {
	err := durable.SyncDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
