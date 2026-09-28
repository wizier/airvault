package engine

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wizier/airvault/internal/ios/backup2"
	"github.com/wizier/airvault/internal/objectstore"
)

const (
	maxBackupPath      = 4096
	maxBackupComponent = 255
	maxBackupDepth     = 128
	deviceStagingDir   = ".b"
	assumedFreeSpace   = 1 << 50
)

var errUnsafePath = errors.New("rejected unsafe mobilebackup2 host path")

// backupStorage serves device paths from a snapshot session. The device may
// address only its source and its ".b" staging area; any other path is
// refused and latched, failing the transfer as an integrity violation.
type backupStorage struct {
	store   *objectstore.Store
	session *objectstore.Session
	source  string

	mu        sync.Mutex
	violation error
}

func (s *backupStorage) Violation() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.violation
}

// key maps a device path to a snapshot key. Like the reference hosts it
// reads the path as relative, dropping empty, "." and ".." components.
func (s *backupStorage) key(devicePath string) (string, error) {
	key, err := backupKey(s.source, devicePath)
	if err != nil {
		s.mu.Lock()
		if s.violation == nil {
			s.violation = err
		}
		s.mu.Unlock()
	}
	return key, err
}

func backupKey(source, devicePath string) (string, error) {
	reject := func(reason string) (string, error) {
		return "", fmt.Errorf("%w %q: %s", errUnsafePath, devicePath, reason)
	}
	if len(devicePath) > maxBackupPath {
		return reject("longer than 4096 bytes")
	}
	var parts []string
	for part := range strings.SplitSeq(devicePath, "/") {
		if part == "" || part == "." || part == ".." {
			continue
		}
		if len(part) > maxBackupComponent || !utf8.ValidString(part) || strings.ContainsAny(part, "\\\x00\u2028\u2029") {
			return reject("invalid component")
		}
		parts = append(parts, part)
	}
	switch {
	case len(parts) == 0:
		return reject("the backup root")
	case len(parts) > maxBackupDepth:
		return reject("deeper than 128 components")
	case parts[0] == source && len(parts) > 1 && parts[1] == objectstore.ProtocolDir:
		return reject("the reserved protocol directory")
	case parts[0] == source:
		return strings.Join(parts[1:], "/"), nil
	case parts[0] == deviceStagingDir:
		key := objectstore.ProtocolDir + "/" + strings.Join(parts, "/")
		if len(key) > maxBackupPath {
			return reject("longer than 4096 bytes")
		}
		return key, nil
	}
	return reject("outside the backup source")
}

func (s *backupStorage) FreeSpace() uint64 {
	free, err := s.store.FreeSpace()
	if err != nil {
		return assumedFreeSpace
	}
	return free
}

func (s *backupStorage) Open(path string) (io.ReadCloser, error) {
	key, err := s.key(path)
	if err != nil {
		return nil, err
	}
	return s.session.Open(key)
}

func (s *backupStorage) Create(path string) (backup2.FileWriter, error) {
	key, err := s.key(path)
	if err != nil {
		return nil, err
	}
	writer, err := s.session.Create(key)
	if err != nil {
		return nil, err
	}
	return writer, nil
}

func (s *backupStorage) MakeDirAll(path string) error {
	return s.with(path, s.session.MakeDirAll)
}

func (s *backupStorage) Remove(path string) error {
	return s.with(path, s.session.Remove)
}

func (s *backupStorage) Rename(from, to string) error {
	return s.withPair(from, to, s.session.Rename)
}

func (s *backupStorage) Copy(from, to string) error {
	return s.withPair(from, to, s.session.Copy)
}

func (s *backupStorage) Exists(path string) bool {
	key, err := s.key(path)
	return err == nil && s.session.Exists(key)
}

func (s *backupStorage) List(path string) ([]backup2.Entry, error) {
	key, err := s.key(path)
	if err != nil {
		return nil, err
	}
	entries, err := s.session.List(key)
	if err != nil {
		return nil, err
	}
	listing := make([]backup2.Entry, len(entries))
	for i, entry := range entries {
		listing[i] = backup2.Entry{Name: entry.Name, Dir: entry.Dir, Size: entry.Size}
		if entry.ModifiedUnix > 0 {
			listing[i].Modified = time.Unix(entry.ModifiedUnix, 0)
		}
	}
	return listing, nil
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
