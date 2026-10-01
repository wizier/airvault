// Package library keeps the backups: the snapshots in the object store and
// the catalog that lists them, consistent across runs, crashes and deletions.
// It is their only writer. The caller holds the source's lease: admitting an
// operation is not the library's job.
package library

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
	"github.com/wizier/airvault/internal/storage"
)

// ErrIncomplete marks a restore point whose backup cannot be opened as a
// complete one.
var ErrIncomplete = errors.New("backup is not complete")

type Library struct {
	objects *objectstore.Store
	catalog *storage.Store
	changed func(source string) // the source's restore points or footprint changed
	wg      sync.WaitGroup      // background reclamation
}

func New(objects *objectstore.Store, catalog *storage.Store, changed func(source string)) *Library {
	return &Library{objects: objects, catalog: catalog, changed: changed}
}

// Wait returns once background reclamation is over.
func (l *Library) Wait() { l.wg.Wait() }

// Summary is each source's restore points, latest backup and footprint, read
// from the catalog alone.
func (l *Library) Summary(ctx context.Context) (map[string]storage.SourceSummary, error) {
	return l.catalog.Backup.SummaryBySource(ctx)
}

func (l *Library) RestorePoints(ctx context.Context, source string) ([]model.Backup, error) {
	return l.catalog.Backup.ListCompleteBySource(ctx, source)
}

func (l *Library) AllRestorePoints(ctx context.Context) ([]model.Backup, error) {
	return l.catalog.Backup.ListComplete(ctx)
}

// Lookup reads a restore point's catalog row. The id comes from a user, so an
// unknown one is a validation error.
func (l *Library) Lookup(ctx context.Context, id string) (*model.Backup, error) {
	if id == "" {
		return nil, &domain.ValidationError{Code: "snapshot_required", Message: "a backup snapshot must be selected"}
	}
	snapshot, err := l.catalog.Backup.Get(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, snapshotNotFound()
	}
	return snapshot, err
}

func snapshotNotFound() error {
	return &domain.ValidationError{Code: "snapshot_not_found", Message: "the selected backup snapshot no longer exists"}
}

func snapshotDamaged() error {
	return &domain.ValidationError{Code: "backup_damaged", Message: "the selected backup is damaged; run an integrity check or delete it"}
}

// Open reads a restore point as an iOS backup; one that is not complete is
// ErrIncomplete.
func (l *Library) Open(ctx context.Context, id string) (*iosbackup.Backup, error) {
	snapshot, err := l.Lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	if snapshot.Damage != "" {
		return nil, snapshotDamaged()
	}
	view, err := l.objects.OpenSnapshot(snapshot.SourceUDID, snapshot.ID)
	if errors.Is(err, fs.ErrNotExist) {
		// A deletion won the race: the row outlived its manifest.
		return nil, snapshotNotFound()
	}
	if err != nil {
		l.NoticeDamage(ctx, snapshot.SourceUDID, err)
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	backup, err := iosbackup.Open(view)
	if err != nil {
		l.NoticeDamage(ctx, snapshot.SourceUDID, err)
		return nil, fmt.Errorf("%w: %w", ErrIncomplete, err)
	}
	return backup, nil
}

// Project is the catalog row of a snapshot that holds a complete backup.
func Project(view *objectstore.Snapshot) (model.Backup, error) {
	backup, err := iosbackup.Open(view)
	if err != nil {
		return model.Backup{}, err
	}
	return model.Backup{
		ID: view.ID(), SourceUDID: view.Source(),
		SizeBytes: view.SizeBytes(),
		Encrypted: backup.Encrypted, IOSVersion: backup.IOSVersion,
		DeviceName: backup.Info.DeviceName, ProductType: backup.Info.ProductType,
		CreatedAt: view.CreatedUnix(),
	}, nil
}

type Export struct {
	*objectstore.Tar
	Name    string
	damaged func(error)
}

func (e *Export) Read(p []byte) (int, error) {
	n, err := e.Tar.Read(p)
	if errors.Is(err, objectstore.ErrIntegrity) {
		e.damaged(err)
	}
	return n, err
}

// Export packs a restore point as a tar named after the phone, around a
// "<UDID>-<date>" folder (server time) that cannot merge into MobileSync/Backup.
// No lease: only a deletion can cut it short. Damage it finds goes to damaged.
func (l *Library) Export(ctx context.Context, id string, damaged func(source string, err error)) (*Export, error) {
	snapshot, err := l.catalog.Backup.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if snapshot.Damage != "" {
		return nil, snapshotDamaged()
	}
	view, err := l.objects.OpenSnapshot(snapshot.SourceUDID, snapshot.ID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	created := time.Unix(snapshot.CreatedAt, 0)
	archive, err := view.Tar(snapshot.SourceUDID + "-" + created.Format("20060102-150405"))
	if err != nil {
		return nil, fmt.Errorf("pack snapshot: %w", err)
	}
	name := cmp.Or(snapshot.DeviceName, snapshot.SourceUDID)
	return &Export{Tar: archive, Name: fmt.Sprintf("AirVault-%s-%s.tar", name, created.Format("2006-01-02-1504")),
		damaged: func(err error) { damaged(snapshot.SourceUDID, err) }}, nil
}
