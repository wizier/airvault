package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
)

// A backup enters the library in three steps: Begin a snapshot on top of
// LatestBase, let the device fill and seal it, then Publish it or Discard it.

// LatestBase is the newest restore point usable as an incremental base, nil
// before the first backup. A damaged newest one is skipped, but when none is
// usable the backup fails rather than silently start over with a full copy.
func (l *Library) LatestBase(ctx context.Context, source string) (*iosbackup.Backup, error) {
	snapshots, err := l.catalog.Backup.ListCompleteBySource(ctx, source)
	if err != nil || len(snapshots) == 0 {
		return nil, err
	}
	for _, snapshot := range snapshots {
		view, err := l.objects.OpenSnapshot(source, snapshot.ID)
		if err == nil {
			var backup *iosbackup.Backup
			if backup, err = iosbackup.Open(view); err == nil {
				return backup, nil
			}
		}
		slog.WarnContext(ctx, "backup snapshot is not usable as an incremental base",
			"snapshot_id", snapshot.ID, "source", source, "error", err)
	}
	return nil, errors.New("no complete backup snapshot is usable as an incremental base")
}

// Begin starts snapshot id of source on top of base, nil for a full backup.
func (l *Library) Begin(source, id string, base *iosbackup.Backup) (*objectstore.Session, error) {
	var view *objectstore.View
	if base != nil {
		view = base.View
	}
	return l.objects.BeginSnapshot(source, id, view)
}

// Publish makes a sealed snapshot a restore point with row as its catalog
// entry; added is what it pooled. A failure past the manifest's rename still
// leaves a restore point, so it is recovered instead of discarded.
func (l *Library) Publish(ctx context.Context, staged *objectstore.StagingView, row model.Backup, added int64) error {
	if _, err := l.objects.Publish(staged); err != nil {
		recovered, settleErr := l.settle(ctx, row)
		if recovered && settleErr == nil {
			slog.WarnContext(ctx, "backup: publication recovered after error", "snapshot_id", row.ID, "error", err)
			return nil
		}
		return errors.Join(err, settleErr)
	}
	// The footprint grows by the pooled payload plus the manifest itself.
	var delta *int64
	if manifestBytes, err := l.objects.SnapshotManifestBytes(row.SourceUDID, row.ID); err == nil {
		delta = new(added + manifestBytes)
	}
	if err := l.recordSnapshot(ctx, row, delta); err != nil {
		if _, getErr := l.catalog.Backup.Get(ctx, row.ID); getErr != nil {
			// The published manifest is the durable commit: its objects are never
			// discarded because SQLite failed, and startup rebuilds the catalog.
			return fmt.Errorf("backup was saved, but its catalog entry couldn't be updated: %w", errors.Join(err, getErr))
		}
		slog.WarnContext(ctx, "backup: catalog commit returned an error but is durable", "snapshot_id", row.ID, "error", err)
	}
	return nil
}

// Discard drops an unpublished snapshot; what it pooled goes with the next
// collection if this one fails.
func (l *Library) Discard(ctx context.Context, source, id string) error {
	if err := l.objects.DiscardStaging(source, id); err != nil {
		return err
	}
	if err := l.Collect(context.WithoutCancel(ctx), source); err != nil {
		slog.WarnContext(ctx, "discard snapshot: orphan collection deferred", "source", source, "error", err)
	}
	return nil
}

// settle resolves an interrupted snapshot: a published manifest is the durable
// commit and is recovered with row's run facts; anything else is discarded.
// True only on a complete recovery.
func (l *Library) settle(ctx context.Context, row model.Backup) (bool, error) {
	published, openErr := l.objects.OpenSnapshot(row.SourceUDID, row.ID)
	if openErr == nil {
		recovered, err := Project(published)
		if err != nil {
			return false, fmt.Errorf("validate interrupted published snapshot: %w", err)
		}
		// The rename already committed; complete the durability boundary and
		// clear the mutable envelope left by the crash.
		if err := l.objects.FinishPublication(published); err != nil {
			return false, fmt.Errorf("finish interrupted snapshot publication: %w", err)
		}
		fctx := context.WithoutCancel(ctx)
		recovered.StartedAt, recovered.TransferredBytes = row.StartedAt, row.TransferredBytes
		// Its delta is unknown, so the collection below measures the source.
		if err := l.recordSnapshot(fctx, recovered, nil); err != nil {
			return false, fmt.Errorf("recover published snapshot: %w", err)
		}
		if err := l.Collect(fctx, row.SourceUDID); err != nil {
			slog.WarnContext(fctx, "recovered snapshot: object collection deferred",
				"source", row.SourceUDID, "error", err)
		}
		return true, nil
	}
	if !errors.Is(openErr, fs.ErrNotExist) {
		// The run is over either way, so the envelope goes whatever kept the
		// manifest unreadable: a stranded one disables the source's collection.
		inspectErr := fmt.Errorf("inspect interrupted snapshot: %w", openErr)
		if err := l.objects.DiscardStaging(row.SourceUDID, row.ID); err != nil {
			return false, errors.Join(inspectErr, fmt.Errorf("discard interrupted staging: %w", err))
		}
		return false, inspectErr
	}
	if err := l.Discard(ctx, row.SourceUDID, row.ID); err != nil {
		return false, fmt.Errorf("discard interrupted snapshot: %w", err)
	}
	return false, nil
}
