package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/objectstore"
)

// BackupExport is a restore point packed as a Finder-format backup: a tar of
// one "<UDID>-<date>" folder that Finder, Apple Devices and backup readers open.
type BackupExport struct {
	*objectstore.Tar
	Name string // attachment file name
}

// OpenBackupExport packs a restore point for download. A snapshot's logical
// paths already are Finder's backup layout, so its files go in unchanged.
//
// It takes no lease: a published snapshot never changes and object collection
// keeps everything it references, so backups run on meanwhile. Only deleting
// this very snapshot can cut a download short.
func (s *Service) OpenBackupExport(ctx context.Context, snapshotID string) (*BackupExport, error) {
	snapshot, err := s.store.Backup.Get(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	view, err := s.objects.OpenSnapshot(snapshot.SourceUDID, snapshot.ID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	// The date suffix keeps an unpacked copy from merging into a backup of the
	// same phone already in MobileSync/Backup.
	created := time.Unix(snapshot.CreatedAt, 0)
	archive, err := view.Tar(snapshot.SourceUDID + "-" + created.Format("20060102-150405"))
	if err != nil {
		return nil, fmt.Errorf("pack snapshot: %w", err)
	}
	name := snapshot.DeviceName
	if name == "" {
		name = snapshot.SourceUDID
	}
	return &BackupExport{
		Tar:  archive,
		Name: fmt.Sprintf("AirVault-%s-%s.tar", name, created.Format("2006-01-02")),
	}, nil
}
