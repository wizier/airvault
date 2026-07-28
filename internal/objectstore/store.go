package objectstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wizier/airvault/internal/domain"

	"github.com/google/uuid"
)

// Store owns object manifests and immutable file objects below BackupDir,
// and owns that root exclusively.
type Store struct {
	root       string
	lockFile   *os.File
	syncFor    func() error       // test seam for failures before the atomic rename
	syncDirFor func(string) error // test seam for failures after the atomic rename
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
	return &Store{root: absolute, lockFile: lockFile}, nil
}

// Close releases this process's exclusive ownership of the object-store root.
func (s *Store) Close() error {
	if s.lockFile == nil {
		return nil
	}
	file := s.lockFile
	s.lockFile = nil
	return releaseStoreLock(file)
}

// sourcePath resolves a source's subtree, refusing an invalid name and any path
// that reaches it through a symlink. Parts are the store's own literals or
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
	if err := s.preparePublication(); err != nil {
		return nil, err
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

// RemoveSnapshot atomically removes one manifest from the live reachability
// roots.
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

// UnpublishSource drops every restore point of one source together with any
// interrupted run's envelope. Nothing in the tree is reachable once it returns.
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

// RemoveSourceTree removes a source's whole subtree, whatever shape its pool is
// in. It reclaims what UnpublishSource left unreachable.
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
	if err := s.syncPublishedNamespace(source); err != nil {
		return err
	}
	if err := s.DiscardStaging(source, published.manifest.SnapshotID); err != nil {
		return fmt.Errorf("remove published staging directory: %w", err)
	}
	return nil
}

func (s *Store) preparePublication() error {
	syncStore := s.Sync
	if s.syncFor != nil {
		syncStore = s.syncFor
	}
	if err := syncStore(); err != nil {
		return fmt.Errorf("sync published snapshot contents: %w", err)
	}
	return nil
}

func (s *Store) syncPublishedNamespace(source string) error {
	syncDir := syncDirectory
	if s.syncDirFor != nil {
		syncDir = s.syncDirFor
	}
	sourceRoot, err := s.sourcePath(source)
	if err != nil {
		return err
	}
	if err := syncDir(filepath.Join(sourceRoot, "snapshots")); err != nil {
		return fmt.Errorf("sync published object manifest: %w", err)
	}
	if err := syncDir(sourceRoot); err != nil {
		return fmt.Errorf("sync published snapshot directory: %w", err)
	}
	return nil
}

// DiscardStaging unconditionally drops one snapshot's mutable envelope — a
// stranded one blocks every later collection for the source.
func (s *Store) DiscardStaging(source, snapshotID string) error {
	if err := validateSnapshotIdentity(source, snapshotID); err != nil {
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

// ReconcileStaging resolves every filesystem transaction: published manifests
// finish their durability boundary, unpublished ones are discarded.
func (s *Store) ReconcileStaging() error {
	sources, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, sourceEntry := range sources {
		if !sourceEntry.IsDir() || domain.ValidateSource(sourceEntry.Name()) != nil {
			continue
		}
		if err := s.ReconcileSourceStaging(sourceEntry.Name()); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileSourceStaging is ReconcileStaging for one device — the runtime entry
// point when a run dies without unwinding through its own cleanup.
func (s *Store) ReconcileSourceStaging(source string) error {
	stagingRoot, err := s.sourcePath(source, "staging")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(stagingRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		snapshotID := entry.Name()
		if hiddenEntry(snapshotID) {
			continue
		}
		if !entry.IsDir() || validateSnapshotID(snapshotID) != nil {
			return fmt.Errorf("unexpected staging entry %q for source %q", snapshotID, source)
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

// ListSources returns every source (UDID) subtree present in the store.
func (s *Store) ListSources() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sources []string
	for _, entry := range entries {
		if entry.IsDir() && domain.ValidateSource(entry.Name()) == nil {
			sources = append(sources, entry.Name())
		}
	}
	sort.Strings(sources)
	return sources, nil
}

// hiddenEntry reports Finder/SMB metadata droppings (.DS_Store, AppleDouble).
// The store's own names are never dot-prefixed, so listings skip these instead
// of failing closed; anything else unexpected still aborts.
func hiddenEntry(name string) bool {
	return strings.HasPrefix(name, ".")
}

type snapshotManifestFile struct {
	id   string
	size int64
}

// listSnapshotManifests is the single definition of what belongs in a source's
// snapshots directory: catalog rebuild, object liveness and the footprint all
// read it and must agree.
func (s *Store) listSnapshotManifests(source string) ([]snapshotManifestFile, error) {
	dir, err := s.sourcePath(source, "snapshots")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var manifests []snapshotManifestFile
	for _, entry := range entries {
		name := entry.Name()
		if hiddenEntry(name) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || !strings.HasSuffix(name, ".json") {
			return nil, fmt.Errorf("unexpected snapshots manifest entry %q", name)
		}
		id := strings.TrimSuffix(name, ".json")
		if err := validateSnapshotID(id); err != nil {
			return nil, fmt.Errorf("unexpected snapshots manifest %q: %w", name, err)
		}
		manifests = append(manifests, snapshotManifestFile{id: id, size: info.Size()})
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].id < manifests[j].id })
	return manifests, nil
}

// ListSnapshotIDs returns the ids of a source's published (complete)
// manifests on disk. It underpins catalog rebuild and object liveness.
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

// resolveManifest resolves a stored manifest-relative path; the containment
// check lives in rejectSymlinkTraversal.
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
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
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

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func syncExistingDirectory(directory string) error {
	err := syncDirectory(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
