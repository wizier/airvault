package objectstore

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/durable"
)

var ErrIntegrity = errors.New("backup data failed verification")

var errReadOnly = errors.New("restore source is read-only")

// ProtocolDir holds the device's staging files, which never reach the snapshot.
const ProtocolDir = ".airvault-protocol"

// Session is a backup rewriting a copy of its base snapshot, or a read-only
// restore. Failures that make the result untrustworthy are latched and fail
// Seal; refusals of single requests are only returned.
type Session struct {
	store      *Store
	source     string
	snapshotID string
	staging    string // staging/<id>; empty for a restore

	mu      sync.Mutex
	tree    *tree
	failure error
	writers int
	written map[string]bool // objects this run added to the pool
	shards  map[string]string
}

// BeginSnapshot starts a snapshot of source that begins as a copy of base,
// nil for a full one. Its staging directory holds object collection off the
// source until Publish or DiscardStaging.
func (s *Store) BeginSnapshot(source, snapshotID string, base *View) (*Session, error) {
	if err := validateSnapshotIdentity(source, snapshotID); err != nil {
		return nil, err
	}
	if base != nil && base.Source() != source {
		return nil, fmt.Errorf("base snapshot %s belongs to %s, not %s", base.ID(), base.Source(), source)
	}
	staging, err := s.sourcePath(source, "staging", snapshotID)
	if err != nil {
		return nil, err
	}
	published, err := s.resolveManifest(snapshotManifestRelative(source, snapshotID))
	if err != nil {
		return nil, err
	}
	for _, path := range []string{staging, published} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: snapshot %s already exists", ErrIntegrity, snapshotID)
		}
	}
	tree := newTree()
	if base != nil {
		tree = treeFromEntries(base.manifest.Entries)
	}
	if err := os.MkdirAll(filepath.Join(staging, "objects"), 0o755); err != nil {
		return nil, fmt.Errorf("create staging: %w", err)
	}
	return &Session{store: s, source: source, snapshotID: snapshotID, staging: staging, tree: tree,
		written: map[string]bool{}, shards: map[string]string{}}, nil
}

// Session is a read-only session over the snapshot, for serving a restore.
func (v *View) Session() *Session {
	return &Session{store: v.store, source: v.Source(), snapshotID: v.ID(), tree: treeFromEntries(v.manifest.Entries)}
}

func (s *Session) Source() string { return s.source }

// FreeSpace is what the store's volume has left for new objects.
func (s *Session) FreeSpace() (uint64, error) { return s.store.FreeSpace() }

func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *Session) fail(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil {
		s.failure = err
	}
	return err
}

type Entry struct {
	Name         string
	Dir          bool
	Size         int64
	ModifiedUnix int64
}

func (s *Session) Exists(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tree.get(key) != nil
}

func (s *Session) List(key string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.tree.get(key)
	if dir == nil || !dir.isDir() {
		return nil, fmt.Errorf("path %q is not a directory", key)
	}
	entries := make([]Entry, 0, len(dir.children))
	for name, child := range dir.children {
		entries = append(entries, Entry{Name: name, Dir: child.isDir(), Size: child.size, ModifiedUnix: child.modifiedUnix})
	}
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.Name, b.Name) })
	return entries, nil
}

// Open verifies content against its address as it is read. A missing key is
// fs.ErrNotExist: a first backup asks for state files it lacks.
func (s *Session) Open(key string) (io.ReadCloser, error) {
	s.mu.Lock()
	node := s.tree.get(key)
	s.mu.Unlock()
	switch {
	case node == nil:
		return nil, fs.ErrNotExist
	case node.isDir():
		return nil, s.fail(fmt.Errorf("%w: %q is a directory", ErrIntegrity, key))
	}
	return s.store.openReader(s.source, key, node.objectRef, node.size, s.fail)
}

// Writer leaves nothing behind unless Commit succeeds.
type Writer struct {
	session *Session
	key     string
	temp    *os.File
	buffer  *bufio.Writer
	digest  hash.Hash
	size    int64
	err     error
}

func (s *Session) Create(key string) (*Writer, error) {
	if s.staging == "" {
		return nil, errReadOnly
	}
	temp, err := os.CreateTemp(filepath.Join(s.staging, "objects"), "*.tmp")
	if err != nil {
		return nil, s.fail(fmt.Errorf("create object: %w", err))
	}
	s.mu.Lock()
	s.writers++
	s.mu.Unlock()
	return &Writer{session: s, key: key, temp: temp, buffer: bufio.NewWriterSize(temp, 256<<10), digest: sha256.New()}, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.buffer.Write(p)
	w.digest.Write(p[:n])
	w.size += int64(n)
	if err != nil {
		w.err = w.session.fail(fmt.Errorf("write %q: %w", w.key, err))
	}
	return n, w.err
}

func (w *Writer) Commit() error {
	defer w.done()
	err := w.err
	if err == nil {
		if err = w.buffer.Flush(); err != nil {
			err = w.session.fail(fmt.Errorf("write %q: %w", w.key, err))
		}
	}
	// Closed before the rename or removal: SMB refuses both on an open file.
	if closeErr := w.temp.Close(); err == nil && closeErr != nil {
		err = w.session.fail(fmt.Errorf("write %q: %w", w.key, closeErr))
	}
	if err != nil {
		_ = os.Remove(w.temp.Name())
		return err
	}
	ref := hex.EncodeToString(w.digest.Sum(nil))
	created, err := w.session.pool(w.temp.Name(), ref, w.size)
	if err != nil {
		_ = os.Remove(w.temp.Name())
		return w.session.fail(err)
	}
	s := w.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if created {
		s.written[ref] = true
	}
	if err := s.tree.insertFile(w.key, ref, w.size, time.Now().Unix()); err != nil {
		err = fmt.Errorf("%w: %v", ErrIntegrity, err)
		if s.failure == nil {
			s.failure = err
		}
		return err
	}
	return nil
}

func (w *Writer) Abort() {
	defer w.done()
	_ = w.temp.Close()
	_ = os.Remove(w.temp.Name())
}

func (w *Writer) done() {
	w.session.mu.Lock()
	w.session.writers--
	w.session.mu.Unlock()
}

// pool keeps a stored copy of the right length: renaming this unsynced file
// over it could lose bytes if power fails. A wrong length is damage it heals.
func (s *Session) pool(temp, ref string, size int64) (created bool, err error) {
	shard := ref[:objectPrefixLength]
	s.mu.Lock()
	dir, ready := s.shards[shard]
	s.mu.Unlock()
	if !ready {
		// Resolved once per shard: the symlink walk is not paid per object.
		if dir, err = s.store.sourcePath(s.source, "objects", shard); err != nil {
			return false, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, err
		}
		s.mu.Lock()
		s.shards[shard] = dir
		s.mu.Unlock()
	}
	target := filepath.Join(dir, ref)
	info, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, os.Rename(temp, target)
	case err != nil:
		return false, err
	case !info.Mode().IsRegular():
		return false, fmt.Errorf("%w: pool object %s is not a regular file", ErrIntegrity, ref)
	case info.Size() == size:
		return false, os.Remove(temp)
	}
	return false, os.Rename(temp, target)
}

func (s *Session) MakeDirAll(key string) error {
	if s.staging == "" {
		return errReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.tree.ensureDir(key, time.Now().Unix())
	return err
}

func (s *Session) Remove(key string) error {
	if s.staging == "" {
		return errReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" || s.tree.remove(key) == nil {
		return fmt.Errorf("path %q does not exist", key)
	}
	return nil
}

func (s *Session) Rename(from, to string) error {
	if s.staging == "" {
		return errReadOnly
	}
	if from == "" || to == "" {
		return errors.New("cannot rename the snapshot root")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tree.rename(from, to, time.Now().Unix())
}

// Copy merges recursively; a missing source, a copy into itself or a
// conflict is skipped: the protocol cannot report a per-item failure.
func (s *Session) Copy(src, dst string) error {
	if s.staging == "" {
		return errReadOnly
	}
	if src == "" || dst == "" {
		return errors.New("cannot copy the snapshot root")
	}
	if dst == src || len(dst) > len(src) && dst[:len(src)+1] == src+"/" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if source := s.tree.get(src); source != nil {
		s.tree.merge(dst, source, time.Now().Unix())
	}
	return nil
}

// Seal writes the staging manifest and returns the snapshot for Publish and
// the bytes this backup added to the pool.
func (s *Session) Seal(ctx context.Context) (*StagingView, int64, error) {
	if s.staging == "" {
		return nil, 0, errReadOnly
	}
	s.mu.Lock()
	failure, writers := s.failure, s.writers
	s.tree.remove(ProtocolDir)
	entries, written := s.tree.entries(), s.written
	s.written = map[string]bool{} // Seal owns these now
	s.mu.Unlock()
	switch {
	case failure != nil:
		return nil, 0, failure
	case writers != 0:
		return nil, 0, fmt.Errorf("%w: %d files still being written", ErrIntegrity, writers)
	}
	manifest := &manifestProjection{
		Version:    formatVersion,
		SourceUDID: s.source,
		SnapshotID: s.snapshotID,
		Entries:    entries,
	}
	relative := stagingManifestRelative(s.source, s.snapshotID)
	facts, err := inspectManifestEntries(relative, manifest)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	manifest.SizeBytes, manifest.EntriesSHA256 = facts.sizeBytes, facts.entriesSHA256
	added, err := s.prune(ctx, entries, written)
	if err != nil {
		return nil, 0, err
	}
	manifest.CreatedUnix = time.Now().Unix()
	if err := validateManifestHeader(relative, manifest); err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if err := writeManifest(filepath.Join(s.staging, "manifest.json"), manifest); err != nil {
		return nil, 0, err
	}
	return &StagingView{View{store: s.store, relative: relative, manifest: manifest}}, added, nil
}

// prune deletes what this run wrote but the tree dropped, with no pool scan.
func (s *Session) prune(ctx context.Context, entries map[string]manifestEntry, written map[string]bool) (int64, error) {
	var added int64
	referenced := map[string]bool{}
	for _, entry := range entries {
		if entry.Kind != entryFile || referenced[entry.ObjectRef] {
			continue
		}
		referenced[entry.ObjectRef] = true
		if written[entry.ObjectRef] {
			added += entry.Size
		}
	}
	for ref := range written {
		if referenced[ref] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		path, err := s.store.resolveObjectRef(s.source, ref)
		if err == nil {
			err = os.Remove(path)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("remove superseded object %s: %w", ref, err)
		}
	}
	return added, nil
}

func writeManifest(path string, manifest *manifestProjection) error {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(manifest); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if encoded.Len() > maxManifestSize {
		return fmt.Errorf("%w: manifest of %d bytes", ErrIntegrity, encoded.Len())
	}
	return durable.WriteFile(path, encoded.Bytes(), 0o644)
}
