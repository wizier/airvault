package engine

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/ios/backup2"
	"github.com/wizier/airvault/internal/objectstore"
)

const (
	deviceStagingDir = ".b"
	assumedFreeSpace = 1 << 50
)

var errUnsafePath = errors.New("rejected unsafe mobilebackup2 host path")

// backupStorage serves device paths from a snapshot session. The device may
// address only its source and its ".b" staging area; any other path is
// refused and latched, failing the transfer as an integrity violation.
type backupStorage struct {
	session *objectstore.Session

	mu        sync.Mutex
	violation error
}

func (s *backupStorage) Violation() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.violation
}

func (s *backupStorage) key(devicePath string) (string, error) {
	key, err := backupKey(s.session.Source(), devicePath)
	if err != nil {
		s.mu.Lock()
		if s.violation == nil {
			s.violation = err
		}
		s.mu.Unlock()
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
		if key == objectstore.ProtocolDir || strings.HasPrefix(key, objectstore.ProtocolDir+"/") {
			return reject("the reserved protocol directory")
		}
	case parts[0] == deviceStagingDir:
		key = objectstore.ProtocolDir + "/" + strings.Join(parts, "/")
	default:
		return reject("outside the backup source")
	}
	if key != "" && !objectstore.ValidKey(key) {
		return reject("not a valid snapshot key")
	}
	return key, nil
}

func (s *backupStorage) FreeSpace() uint64 {
	free, err := s.session.FreeSpace()
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
