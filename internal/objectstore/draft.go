package objectstore

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Draft is a snapshot being written: it starts as a copy of its base and is
// sealed for Publish. Failures that make the result untrustworthy are latched
// and fail Seal; refusals of single requests are only returned.
type Draft struct {
	store      *Store
	source     string
	snapshotID string
	staging    string // staging/<id>

	mu      sync.Mutex
	tree    *tree
	failure error
	writers int
	shards  map[string]string
}

// BeginSnapshot starts a snapshot of source that begins as a copy of base,
// nil for a full one. Its staging directory holds object collection off the
// source until Publish or Discard.
func (s *Store) BeginSnapshot(source, snapshotID string, base *Snapshot) (*Draft, error) {
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
	return &Draft{store: s, source: source, snapshotID: snapshotID, staging: staging, tree: tree,
		shards: map[string]string{}}, nil
}

func (d *Draft) Source() string { return d.source }

// FreeSpace is what the store's volume has left for new objects.
func (d *Draft) FreeSpace() (uint64, error) { return d.store.freeSpace() }

func (d *Draft) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failure
}

func (d *Draft) fail(err error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failure == nil {
		d.failure = err
	}
	return err
}

func (d *Draft) Exists(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tree.get(key) != nil
}

func (d *Draft) List(key string) ([]Entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tree.list(key)
}

// Open verifies content against its address as it is read. A missing key is
// fs.ErrNotExist: a first backup asks for state files it lacks.
func (d *Draft) Open(key string) (io.ReadCloser, error) {
	d.mu.Lock()
	node := d.tree.get(key)
	d.mu.Unlock()
	switch {
	case node == nil:
		return nil, fs.ErrNotExist
	case node.isDir():
		return nil, d.fail(fmt.Errorf("%w: %q is a directory", ErrIntegrity, key))
	}
	return d.store.openReader(d.source, key, node.objectRef, node.size, d.fail)
}

// Writer leaves nothing behind unless Commit succeeds.
type Writer struct {
	draft  *Draft
	key    string
	temp   *os.File
	buffer *bufio.Writer
	digest hash.Hash
	size   int64
	err    error
}

func (d *Draft) Create(key string) (*Writer, error) {
	// Not CreateTemp: its 0600 would ignore the umask set for the share.
	temp, err := os.OpenFile(filepath.Join(d.staging, "objects", rand.Text()+".tmp"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, d.fail(fmt.Errorf("create object: %w", err))
	}
	d.mu.Lock()
	d.writers++
	d.mu.Unlock()
	return &Writer{draft: d, key: key, temp: temp, buffer: bufio.NewWriterSize(temp, 256<<10), digest: sha256.New()}, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.buffer.Write(p)
	w.digest.Write(p[:n])
	w.size += int64(n)
	if err != nil {
		w.err = w.draft.fail(fmt.Errorf("write %q: %w", w.key, err))
	}
	return n, w.err
}

func (w *Writer) Commit() error {
	defer w.done()
	err := w.err
	if err == nil {
		if err = w.buffer.Flush(); err != nil {
			err = w.draft.fail(fmt.Errorf("write %q: %w", w.key, err))
		}
	}
	// Closed before the rename or removal: SMB refuses both on an open file.
	if closeErr := w.temp.Close(); err == nil && closeErr != nil {
		err = w.draft.fail(fmt.Errorf("write %q: %w", w.key, closeErr))
	}
	if err != nil {
		_ = os.Remove(w.temp.Name())
		return err
	}
	ref := hex.EncodeToString(w.digest.Sum(nil))
	if err := w.draft.pool(w.temp.Name(), ref, w.size); err != nil {
		_ = os.Remove(w.temp.Name())
		return w.draft.fail(err)
	}
	d := w.draft
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.tree.insertFile(w.key, ref, w.size, time.Now().Unix()); err != nil {
		err = fmt.Errorf("%w: %v", ErrIntegrity, err)
		if d.failure == nil {
			d.failure = err
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
	w.draft.mu.Lock()
	w.draft.writers--
	w.draft.mu.Unlock()
}

// pool keeps a stored copy of the right length: renaming this unsynced file
// over it could lose bytes if power fails. A wrong length is damage it heals.
func (d *Draft) pool(temp, ref string, size int64) error {
	shard := ref[:objectPrefixLength]
	d.mu.Lock()
	dir, ready := d.shards[shard]
	d.mu.Unlock()
	if !ready {
		// Resolved once per shard: the symlink walk is not paid per object.
		var err error
		if dir, err = d.store.sourcePath(d.source, "objects", shard); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		d.mu.Lock()
		d.shards[shard] = dir
		d.mu.Unlock()
	}
	target := filepath.Join(dir, ref)
	info, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return fmt.Errorf("%w: pool object %s is not a regular file", ErrIntegrity, ref)
	case info.Size() == size:
		return os.Remove(temp)
	}
	return os.Rename(temp, target)
}

func (d *Draft) MakeDirAll(key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.tree.ensureDir(key, time.Now().Unix())
	return err
}

func (d *Draft) Remove(key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if key == "" || d.tree.remove(key) == nil {
		return fmt.Errorf("path %q does not exist", key)
	}
	return nil
}

func (d *Draft) Rename(from, to string) error {
	if from == "" || to == "" {
		return errors.New("cannot rename the snapshot root")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tree.rename(from, to, time.Now().Unix())
}

// Copy merges recursively; a missing source, a copy into itself or a
// conflict is skipped: the protocol cannot report a per-item failure.
func (d *Draft) Copy(src, dst string) error {
	if src == "" || dst == "" {
		return errors.New("cannot copy the snapshot root")
	}
	if dst == src || len(dst) > len(src) && dst[:len(src)+1] == src+"/" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if source := d.tree.get(src); source != nil {
		d.tree.merge(dst, source, time.Now().Unix())
	}
	return nil
}

// Seal writes the staging manifest and returns the snapshot for Publish.
func (d *Draft) Seal(ctx context.Context) (*StagedSnapshot, error) {
	d.mu.Lock()
	failure, writers := d.failure, d.writers
	entries := d.tree.entries()
	d.mu.Unlock()
	switch {
	case failure != nil:
		return nil, failure
	case writers != 0:
		return nil, fmt.Errorf("%w: %d files still being written", ErrIntegrity, writers)
	}
	sealed := &manifest{
		Version:    formatVersion,
		SourceUDID: d.source,
		SnapshotID: d.snapshotID,
		Entries:    entries,
	}
	relative := stagingManifestRelative(d.source, d.snapshotID)
	facts, err := inspectManifestEntries(relative, sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	sealed.SizeBytes, sealed.EntriesSHA256 = facts.sizeBytes, facts.entriesSHA256
	sealed.CreatedUnix = time.Now().Unix()
	if err := validateManifestHeader(relative, sealed); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := writeManifest(filepath.Join(d.staging, "manifest.json"), sealed); err != nil {
		return nil, err
	}
	return &StagedSnapshot{Snapshot{store: d.store, relative: relative, manifest: sealed}}, nil
}
