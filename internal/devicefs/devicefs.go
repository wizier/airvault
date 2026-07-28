package devicefs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/wizier/airvault/internal/engine"
	"golang.org/x/sync/semaphore"
)

const (
	// AFC reads are ordinary shared transfers, not backup-style exclusive
	// sessions. Keep a small per-phone cap so a thumbnail grid cannot open an
	// unbounded number of lockdown services.
	maxConcurrentSessions = 3
	copyBufferSize        = 1 << 20
)

type EntryKind string

const (
	EntryFile      EntryKind = "file"
	EntryDirectory EntryKind = "directory"
)

type Entry struct {
	Name     string    `json:"name"`
	Kind     EntryKind `json:"kind"`
	Size     *int64    `json:"size,omitempty"`
	Modified *int64    `json:"modified,omitempty"`
}

type opener interface {
	OpenAFC(context.Context, engine.DeviceID, engine.AFCSource, string) (engine.AFCSession, error)
}

// Manager owns request-scoped AFC sessions and their per-device concurrency.
type Manager struct {
	opener opener

	mu    sync.Mutex
	gates map[string]*semaphore.Weighted
}

func New(opener opener) *Manager {
	return &Manager{opener: opener, gates: make(map[string]*semaphore.Weighted)}
}

// acquire caps concurrent AFC sessions per phone. The semaphore per UDID is
// created once and kept: the key space is the few devices ever paired.
func (m *Manager) acquire(ctx context.Context, udid string) (func(), error) {
	m.mu.Lock()
	gate := m.gates[udid]
	if gate == nil {
		gate = semaphore.NewWeighted(maxConcurrentSessions)
		m.gates[udid] = gate
	}
	m.mu.Unlock()
	if err := gate.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	return sync.OnceFunc(func() { gate.Release(1) }), nil
}

// Forget drops a device's AFC gate after it is unregistered. Unpair holds the
// exclusive device lease, so no session is using the gate here; a later pairing
// recreates it lazily.
func (m *Manager) Forget(udid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.gates, udid)
}

type Session struct {
	native  engine.AFCSession
	root    Root
	release func()

	mu sync.Mutex
}

func (m *Manager) Open(ctx context.Context, udid string, root Root) (*Session, error) {
	release, err := m.acquire(ctx, udid)
	if err != nil {
		return nil, err
	}
	native, err := m.opener.OpenAFC(ctx, engine.DeviceID(udid), root.source, root.bundleID)
	if err != nil {
		release()
		return nil, err
	}
	return &Session{native: native, root: root, release: release}, nil
}

func (s *Session) Close() error {
	native, release := s.take()
	if native == nil {
		return nil
	}
	err := native.Close()
	release()
	return err
}

func (s *Session) take() (engine.AFCSession, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	native, release := s.native, s.release
	s.native, s.release = nil, nil
	return native, release
}

func (s *Session) current() (engine.AFCSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.native == nil {
		return nil, io.ErrClosedPipe
	}
	return s.native, nil
}

// Names returns a validated, deterministic names-only directory projection.
// Gallery scans use it to avoid one AFC stat request per asset.
func (s *Session) Names(path Path) ([]string, error) {
	physical, err := s.root.physical(path)
	if err != nil {
		return nil, err
	}
	native, err := s.current()
	if err != nil {
		return nil, err
	}
	names, err := native.List(physical)
	if err != nil {
		return nil, err
	}
	filtered := names[:0]
	for _, name := range names {
		if name == "." || name == ".." {
			continue
		}
		if _, err := path.Child(name); err != nil {
			return nil, fmt.Errorf("invalid child name %q: %w", name, err)
		}
		filtered = append(filtered, name)
	}
	sort.Slice(filtered, func(i, j int) bool {
		left, right := filtered[i], filtered[j]
		return lessFold(left, right)
	})
	return filtered, nil
}

func lessFold(left, right string) bool {
	leftFold, rightFold := strings.ToLower(left), strings.ToLower(right)
	if leftFold == rightFold {
		return left < right
	}
	return leftFold < rightFold
}

// List returns one whole directory, every entry statted, directories first then
// case-insensitively by name. Browsed directories are bounded, so one pass
// beats server-side paging state.
func (s *Session) List(path Path) ([]Entry, error) {
	names, err := s.Names(path)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		child, err := path.Child(name)
		if err != nil {
			return nil, err
		}
		info, err := s.Stat(child)
		if err != nil {
			return nil, fmt.Errorf("stat %q: %w", child.String(), err)
		}
		entries = append(entries, info)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].Kind == EntryDirectory, entries[j].Kind == EntryDirectory
		if di != dj {
			return di
		}
		return lessFold(entries[i].Name, entries[j].Name)
	})
	return entries, nil
}

// ListingRevision is an order-sensitive fingerprint of a string listing, used
// as the opaque stable-pagination token for both directory and gallery scans.
func ListingRevision(names []string) string {
	digest := sha256.New()
	var length [4]byte
	for _, name := range names {
		binary.BigEndian.PutUint32(length[:], uint32(len(name)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(name))
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func (s *Session) Stat(path Path) (Entry, error) {
	physical, err := s.root.physical(path)
	if err != nil {
		return Entry{}, err
	}
	native, err := s.current()
	if err != nil {
		return Entry{}, err
	}
	info, err := native.Stat(physical)
	if err != nil {
		return Entry{}, err
	}
	kind := EntryFile
	if info.IsDir {
		kind = EntryDirectory
	}
	entry := Entry{Name: path.Name(), Kind: kind}
	if kind == EntryFile {
		size := info.Size
		entry.Size = &size
	}
	if info.Modified > 0 {
		modified := info.Modified
		entry.Modified = &modified
	}
	return entry, nil
}

func (s *Session) Remove(path Path) error {
	if path.String() == "" {
		return fmt.Errorf("cannot remove the device root")
	}
	physical, err := s.root.physical(path)
	if err != nil {
		return err
	}
	native, err := s.current()
	if err != nil {
		return err
	}
	return native.Remove(physical)
}

// ReadFile reads one whole small file, reusing the session (unlike OpenFile,
// which consumes it for streaming). The bulk path for thumbnails.
func (s *Session) ReadFile(path Path) ([]byte, error) {
	if path.String() == "" {
		return nil, fmt.Errorf("cannot read the device root as a file")
	}
	physical, err := s.root.physical(path)
	if err != nil {
		return nil, err
	}
	native, err := s.current()
	if err != nil {
		return nil, err
	}
	return native.ReadSmall(physical)
}

type File struct {
	native  engine.AFCFile
	release func()
	size    int64
	once    sync.Once
}

func (s *Session) OpenFile(path Path) (*File, error) {
	if path.String() == "" {
		return nil, fmt.Errorf("cannot open the device root as a file")
	}
	physical, err := s.root.physical(path)
	if err != nil {
		return nil, err
	}
	native, release := s.take()
	if native == nil {
		return nil, io.ErrClosedPipe
	}
	nativeFile, err := native.Open(physical)
	if err != nil {
		release()
		return nil, err
	}
	return &File{native: nativeFile, release: release, size: nativeFile.Size()}, nil
}

func (f *File) Read(buffer []byte) (int, error) { return f.native.Read(buffer) }

func (f *File) Size() int64 { return f.size }

func (f *File) Close() error {
	var err error
	f.once.Do(func() {
		err = f.native.Close()
		f.release()
	})
	return err
}

// CopyTo streams the file and verifies the size reported when it was opened.
func (f *File) CopyTo(ctx context.Context, destination io.Writer, onProgress func(done, total int64)) error {
	stopCancellation := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stopCancellation()
	total := f.Size()
	written, copyErr := io.CopyBuffer(&progressWriter{
		writer: destination,
		total:  total,
		update: onProgress,
	}, io.LimitReader(f, total), make([]byte, copyBufferSize))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, copyErr)
	}
	if copyErr != nil {
		return copyErr
	}
	if written != total {
		return fmt.Errorf("copied %d bytes, expected %d: %w", written, total, io.ErrUnexpectedEOF)
	}
	return nil
}

type progressWriter struct {
	writer io.Writer
	total  int64
	done   int64
	update func(done, total int64)
}

func (w *progressWriter) Write(buffer []byte) (int, error) {
	written, err := w.writer.Write(buffer)
	w.done += int64(written)
	if w.update != nil {
		w.update(w.done, w.total)
	}
	return written, err
}
