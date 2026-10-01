package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"

	"github.com/wizier/airvault/internal/ios/backup2"
	"github.com/wizier/airvault/internal/objectstore"
)

const (
	deviceStagingDir = ".b"
	// protocolDir is where a backup keeps the device's staging area: in the
	// draft, so the device can use it, but never in the sealed snapshot.
	protocolDir      = ".airvault-protocol"
	assumedFreeSpace = 1 << 50
)

var (
	errUnsafePath = errors.New("rejected unsafe mobilebackup2 host path")
	errReadOnly   = errors.New("a restore point is read-only")
)

// transferStorage is what a transfer serves the device, and what went wrong
// that the device only heard as an error code.
type transferStorage interface {
	backup2.Storage
	Source() string
	// Failure is the first storage failure: it outranks what the device reports.
	Failure() error
	Violation() error
}

// snapshotFiles is what both directions read: the draft a backup fills, or the
// restore point a restore applies.
type snapshotFiles interface {
	Source() string
	Open(key string) (io.ReadCloser, error)
	List(key string) ([]objectstore.Entry, error)
	Exists(key string) bool
}

// deviceFiles serves a snapshot's files by the device's paths. The device may
// address only its source and its ".b" staging area: any other path is a
// violation. It and any failed read are latched and fail the transfer.
type deviceFiles struct {
	files snapshotFiles

	mu        sync.Mutex
	violation error
	failure   error
}

func (f *deviceFiles) Source() string { return f.files.Source() }

func (f *deviceFiles) Violation() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.violation
}

func (f *deviceFiles) Failure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failure
}

func (f *deviceFiles) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure == nil {
		f.failure = err
	}
}

func (f *deviceFiles) key(devicePath string) (string, error) {
	key, err := backupKey(f.files.Source(), devicePath)
	if err != nil {
		f.mu.Lock()
		if f.violation == nil {
			f.violation = err
		}
		f.mu.Unlock()
	}
	return key, err
}

// backupKey maps a device path to a snapshot key. Like the reference hosts it
// reads the path as relative, dropping empty, "." and ".." components; ""
// is the source itself.
func backupKey(source, devicePath string) (string, error) {
	reject := func(reason string) (string, error) {
		return "", fmt.Errorf("%w %.200q: %s", errUnsafePath, devicePath, reason)
	}
	if len(devicePath) > maxDevicePath {
		return reject("too long")
	}
	var parts []string
	for part := range strings.SplitSeq(devicePath, "/") {
		if part != "" && part != "." && part != ".." {
			parts = append(parts, part)
		}
	}
	var key string
	switch {
	case len(parts) == 0:
		return reject("the backup root")
	case parts[0] == source:
		key = strings.Join(parts[1:], "/")
		if key == protocolDir || strings.HasPrefix(key, protocolDir+"/") {
			return reject("the reserved protocol directory")
		}
	case parts[0] == deviceStagingDir:
		key = protocolDir + "/" + strings.Join(parts, "/")
	default:
		return reject("outside the backup source")
	}
	if key != "" && !objectstore.ValidKey(key) {
		return reject("not a valid snapshot key")
	}
	return key, nil
}

// A missing file is an answer, not a failure: a first backup asks for state
// files it lacks.
func (f *deviceFiles) Open(path string) (io.ReadCloser, error) {
	key, err := f.key(path)
	if err != nil {
		return nil, err
	}
	file, err := f.files.Open(key)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			f.fail(err)
		}
		return nil, err
	}
	return &failingReader{ReadCloser: file, fail: f.fail}, nil
}

func (f *deviceFiles) Exists(path string) bool {
	key, err := f.key(path)
	return err == nil && f.files.Exists(key)
}

func (f *deviceFiles) List(path string) ([]backup2.Entry, error) {
	key, err := f.key(path)
	if err != nil {
		return nil, err
	}
	entries, err := f.files.List(key)
	if err != nil {
		return nil, err
	}
	listing := make([]backup2.Entry, len(entries))
	for i, entry := range entries {
		listing[i] = backup2.Entry{Name: entry.Name, Dir: entry.Dir, Size: entry.Size, Modified: entry.Modified}
	}
	return listing, nil
}

// failingReader latches a failed read, which the device only hears as an
// error code.
type failingReader struct {
	io.ReadCloser
	fail func(error)
}

func (r *failingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		r.fail(err)
	}
	return n, err
}

// restoreStorage serves a restore point, which the device only reads.
type restoreStorage struct{ deviceFiles }

func newRestoreStorage(from *objectstore.Snapshot) *restoreStorage {
	return &restoreStorage{deviceFiles{files: from}}
}

// Nothing is written to the host during a restore.
func (*restoreStorage) FreeSpace() uint64 { return assumedFreeSpace }

func (*restoreStorage) Create(string) (backup2.FileWriter, error) { return nil, errReadOnly }
func (*restoreStorage) MakeDirAll(string) error                   { return errReadOnly }
func (*restoreStorage) Remove(string) error                       { return errReadOnly }
func (*restoreStorage) Rename(string, string) error               { return errReadOnly }
func (*restoreStorage) Copy(string, string) error                 { return errReadOnly }

// backupStorage serves the draft a backup fills.
type backupStorage struct {
	deviceFiles
	draft *objectstore.Draft
}

func newBackupStorage(draft *objectstore.Draft) *backupStorage {
	return &backupStorage{deviceFiles: deviceFiles{files: draft}, draft: draft}
}

// The draft's own failures fail its Seal too; they come first.
func (s *backupStorage) Failure() error {
	return cmp.Or(s.draft.Err(), s.deviceFiles.Failure())
}

func (s *backupStorage) FreeSpace() uint64 {
	free, err := s.draft.FreeSpace()
	if err != nil {
		return assumedFreeSpace
	}
	return free
}

func (s *backupStorage) Create(path string) (backup2.FileWriter, error) {
	key, err := s.key(path)
	if err != nil {
		return nil, err
	}
	writer, err := s.draft.Create(key)
	if err != nil {
		return nil, err
	}
	return writer, nil
}

func (s *backupStorage) MakeDirAll(path string) error {
	return s.with(path, s.draft.MakeDirAll)
}

func (s *backupStorage) Remove(path string) error {
	return s.with(path, s.draft.Remove)
}

func (s *backupStorage) Rename(from, to string) error {
	return s.withPair(from, to, s.draft.Rename)
}

func (s *backupStorage) Copy(from, to string) error {
	return s.withPair(from, to, s.draft.Copy)
}

func (s *backupStorage) with(path string, op func(string) error) error {
	key, err := s.key(path)
	if err != nil {
		return err
	}
	return op(key)
}

func (s *backupStorage) withPair(from, to string, op func(string, string) error) error {
	fromKey, err := s.key(from)
	if err != nil {
		return err
	}
	toKey, err := s.key(to)
	if err != nil {
		return err
	}
	return op(fromKey, toKey)
}

// seal drops the device's staging area and seals the rest of the draft.
func seal(ctx context.Context, draft *objectstore.Draft) (*objectstore.StagedSnapshot, error) {
	if draft.Exists(protocolDir) {
		if err := draft.Remove(protocolDir); err != nil {
			return nil, err
		}
	}
	return draft.Seal(ctx)
}
