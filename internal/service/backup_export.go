package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/objectstore"
)

type BackupExport struct {
	*objectstore.Tar
	Name string
}

// No lease: a published snapshot never changes and collection keeps what it
// references, so only deleting this snapshot can cut a download short. Names
// use the server's time zone (TZ).
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
	name := cmp.Or(snapshot.DeviceName, snapshot.SourceUDID)
	return &BackupExport{
		Tar:  archive,
		Name: fmt.Sprintf("AirVault-%s-%s.tar", name, created.Format("2006-01-02-1504")),
	}, nil
}
