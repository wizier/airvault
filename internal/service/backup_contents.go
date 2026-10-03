package service

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/library"
	"github.com/wizier/airvault/internal/objectstore"
)

// The password's key stretch takes seconds, so the backup being browsed stays
// unlocked until nobody has used it for browseIdle. Unlocking another closes it.
const browseIdle = 15 * time.Minute

type unlockedBackup struct {
	unlocking  sync.Mutex // one password's key stretch at a time: each costs seconds of CPU
	mu         sync.Mutex
	snapshotID string
	source     string
	contents   *iosbackup.Contents // nil when none is unlocked
	idle       *time.Timer
}

func (u *unlockedBackup) get(snapshotID string) *iosbackup.Contents {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.contents == nil || u.snapshotID != snapshotID {
		return nil
	}
	u.idle.Reset(browseIdle)
	return u.contents
}

// put keeps contents as the unlocked backup. When requests race to open the
// same snapshot, the first one opened stays and the others are closed.
func (u *unlockedBackup) put(snapshotID, source string, contents *iosbackup.Contents) *iosbackup.Contents {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.contents != nil && u.snapshotID == snapshotID {
		_ = contents.Close()
		u.idle.Reset(browseIdle)
		return u.contents
	}
	u.closeLocked()
	u.snapshotID, u.source, u.contents = snapshotID, source, contents
	u.idle = time.AfterFunc(browseIdle, func() {
		u.mu.Lock()
		defer u.mu.Unlock()
		if u.contents == contents {
			u.closeLocked()
		}
	})
	return contents
}

func (u *unlockedBackup) closeIf(match func(snapshotID, source string) bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.contents != nil && match(u.snapshotID, u.source) {
		u.closeLocked()
	}
}

// closeLocked lets the requests in flight finish first, without holding u.
func (u *unlockedBackup) closeLocked() {
	if u.contents != nil {
		u.idle.Stop()
		go u.contents.Close()
		u.contents = nil
	}
}

func backupLocked() error {
	return &domain.ValidationError{Code: "backup_locked", Message: "unlock this backup with its password to browse it"}
}

// UnlockBackup opens a restore point's files for browsing; an encrypted one
// needs its password. No lease, as for a download: only a deletion can cut
// browsing short, and it closes what it deletes.
func (s *Service) UnlockBackup(ctx context.Context, snapshotID, password string) error {
	_, err := s.unlockBackup(ctx, snapshotID, password)
	return err
}

func (s *Service) unlockBackup(ctx context.Context, snapshotID, password string) (*iosbackup.Contents, error) {
	backup, err := s.library.Open(ctx, snapshotID)
	if errors.Is(err, library.ErrIncomplete) {
		return nil, &domain.ValidationError{Code: "backup_not_browsable", Message: "the selected backup is not confirmed complete and can't be opened"}
	}
	if err != nil {
		return nil, err
	}
	if backup.Encrypted && password == "" {
		return nil, &domain.ValidationError{Code: "backup_password_required", Message: "this backup is encrypted — its password is required"}
	}
	s.unlocked.unlocking.Lock()
	defer s.unlocked.unlocking.Unlock()
	// An unencrypted backup another request opened meanwhile needs no second copy.
	if contents := s.unlocked.get(snapshotID); contents != nil && !backup.Encrypted {
		return contents, nil
	}
	contents, err := backup.Unlock(ctx, password)
	if errors.Is(err, iosbackup.ErrWrongPassword) {
		return nil, &domain.ValidationError{Code: "invalid_backup_password", Message: "this password does not unlock the selected backup"}
	}
	if err != nil {
		return nil, s.backupError(ctx, backup.Source(), err)
	}
	return s.unlocked.put(snapshotID, backup.Source(), contents), nil
}

// backupContents is the open file list; an unencrypted backup opens on demand.
func (s *Service) backupContents(ctx context.Context, snapshotID string) (*iosbackup.Contents, error) {
	if contents := s.unlocked.get(snapshotID); contents != nil {
		return contents, nil
	}
	row, err := s.library.Lookup(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if row.Encrypted {
		return nil, backupLocked()
	}
	return s.unlockBackup(ctx, snapshotID, "")
}

// backupError records the damage err shows and answers a backup closed for
// idleness mid-request as locked.
func (s *Service) backupError(ctx context.Context, source string, err error) error {
	s.noticeReadDamage(ctx, source, err)
	switch {
	case errors.Is(err, iosbackup.ErrClosed):
		return backupLocked()
	case errors.Is(err, iosbackup.ErrNotStored):
		return &domain.ValidationError{Code: "backup_file_not_stored", Message: "the backup does not hold this file"}
	case errors.Is(err, fs.ErrNotExist):
		return domain.ErrNotFound
	}
	return err
}

type BackupFileDownload struct {
	*io.SectionReader
	reader   iosbackup.Reader
	modified time.Time
}

func (d *BackupFileDownload) ModTime() time.Time { return d.modified }

func (d *BackupFileDownload) Close() { _ = d.reader.Close() }

// openBackupDownload serves reader as a download, recording any damage its
// reads find.
func (s *Service) openBackupDownload(ctx context.Context, source string, reader iosbackup.Reader, modified time.Time) *BackupFileDownload {
	checked := damageReporter{ReaderAt: reader, report: func(err error) { s.noticeReadDamage(ctx, source, err) }}
	return &BackupFileDownload{SectionReader: io.NewSectionReader(checked, 0, reader.Size()),
		reader: reader, modified: modified}
}

type damageReporter struct {
	io.ReaderAt
	report func(error)
}

func (r damageReporter) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, offset)
	if err != nil {
		r.report(err)
	}
	return n, err
}

// noticeReadDamage records damage a read found, unless a backup, check or
// deletion holds the source: its own collection records it.
func (s *Service) noticeReadDamage(ctx context.Context, source string, err error) {
	if !errors.Is(err, objectstore.ErrIntegrity) && !errors.Is(err, objectstore.ErrManifestCorrupt) {
		return
	}
	release, busy := s.ops.acquire("backup download", snapshotReadResource(source))
	if busy != nil {
		return
	}
	defer release()
	s.library.NoticeDamage(ctx, source, err)
}
