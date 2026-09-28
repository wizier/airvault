package engine

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/durable"
	"github.com/wizier/airvault/internal/ios"
)

const (
	pendingDir      = ".pending"
	maxRecordSize   = 4 << 20
	maxIdentitySize = 16 << 10
)

// pairStore keeps AirVault's pairing records in the lockdown directory shared
// with usbmuxd and netmuxd, and under .pending the identity of a pairing
// waiting on Trust.
type pairStore struct {
	root string

	identityMu sync.Mutex // serializes reserving pending identities
}

// pairIdentity is kept across polls so a long-open Trust dialog accepts the
// host that saves the record.
type pairIdentity struct {
	HostID     string `plist:"HostID"`
	SystemBUID string `plist:"SystemBUID"`
}

func (s *pairStore) recordPath(udid string) (string, error) {
	if err := validateUDID(udid); err != nil {
		return "", err
	}
	return filepath.Join(s.root, udid+".plist"), nil
}

func (s *pairStore) identityPath(udid string) (string, error) {
	if err := validateUDID(udid); err != nil {
		return "", err
	}
	return filepath.Join(s.root, pendingDir, udid+".plist"), nil
}

// Load returns nil without a usable record: one that does not parse can never
// open a session, so it counts as absent and the next pairing replaces it. Only
// a file that cannot be read is an error, since it may still hold a valid record.
func (s *pairStore) Load(udid string) (*ios.PairRecord, error) {
	path, err := s.recordPath(udid)
	if err != nil {
		return nil, err
	}
	data, err := readPrivate(path, maxRecordSize)
	if data == nil || err != nil {
		return nil, err
	}
	record, err := ios.ParsePairRecord(data)
	if err != nil {
		slog.Warn("unusable pairing record counts as absent; pairing again replaces it", "path", path, "error", err)
		return nil, nil
	}
	return record, nil
}

func (s *pairStore) Save(udid string, record *ios.PairRecord) error {
	path, err := s.recordPath(udid)
	if err != nil {
		return err
	}
	data, err := record.Marshal()
	if err != nil {
		return fmt.Errorf("encode pairing record: %w", err)
	}
	return writePrivate(path, data)
}

func (s *pairStore) Delete(udid string) error {
	path, err := s.recordPath(udid)
	if err != nil {
		return err
	}
	return removePrivate(path)
}

// Identity returns nil without a reservation; like a record, one that does not
// parse counts as absent.
func (s *pairStore) Identity(udid string) (*pairIdentity, error) {
	path, err := s.identityPath(udid)
	if err != nil {
		return nil, err
	}
	data, err := readPrivate(path, maxIdentitySize)
	if data == nil || err != nil {
		return nil, err
	}
	var identity pairIdentity
	if _, err := plist.Unmarshal(data, &identity); err != nil || identity.HostID == "" || identity.SystemBUID == "" {
		return nil, nil
	}
	return &identity, nil
}

// ReserveIdentity keeps an existing reservation.
func (s *pairStore) ReserveIdentity(udid string, candidate pairIdentity) (pairIdentity, error) {
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	existing, err := s.Identity(udid)
	if err != nil {
		return pairIdentity{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	path, err := s.identityPath(udid)
	if err != nil {
		return pairIdentity{}, err
	}
	data, err := plist.Marshal(candidate, plist.XMLFormat)
	if err != nil {
		return pairIdentity{}, fmt.Errorf("encode pending identity: %w", err)
	}
	return candidate, writePrivate(path, data)
}

func (s *pairStore) DeleteIdentity(udid string) error {
	path, err := s.identityPath(udid)
	if err != nil {
		return err
	}
	return removePrivate(path)
}

func validateUDID(udid string) error {
	if err := domain.ValidateSource(udid); err != nil {
		return &Error{Kind: ErrorInvalidArgument, Detail: fmt.Sprintf("invalid udid %q: %v", udid, err)}
	}
	return nil
}

// readPrivate reads a missing file as nil and refuses symlinks.
func readPrivate(path string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// writePrivate replaces path with a 0600 file in a 0700 directory.
func writePrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return durable.WriteFile(path, data, 0o600)
}

func removePrivate(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return durable.SyncDir(filepath.Dir(path))
}
