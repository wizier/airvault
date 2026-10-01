package objectstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/durable"
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
	store.syncContents = store.syncFilesystem
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
