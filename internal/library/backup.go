package library

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
)

// A backup enters the library in three steps: Begin a snapshot on top of
// LatestBase, let the device fill and seal it, then Publish it or Discard it.

// LatestBase is the newest undamaged restore point that opens, nil for a full
// backup when none is undamaged: the device never resends what it believes the
// base holds. When none opens the backup fails rather than silently go full.
func (l *Library) LatestBase(ctx context.Context, source string) (*iosbackup.Backup, error) {
	snapshots, err := l.catalog.Backup.ListCompleteBySource(ctx, source)
	if err != nil {
		return nil, err
	}
	snapshots = slices.DeleteFunc(snapshots, func(snapshot model.Backup) bool { return snapshot.Damage != "" })
	if len(snapshots) == 0 {
		return nil, nil
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
		l.NoticeDamage(ctx, source, err)
	}
	return nil, errors.New("no complete backup snapshot is usable as an incremental base")
}

// Begin starts snapshot id of source on top of base, nil for a full backup.
func (l *Library) Begin(source, id string, base *iosbackup.Backup) (*objectstore.Draft, error) {
	var view *objectstore.Snapshot
	if base != nil {
		view = base.Snapshot
	}
	return l.objects.BeginSnapshot(source, id, view)
}

// Publish makes a sealed snapshot a restore point with row as its catalog
// entry, then collects the source. A failure past the manifest's rename still
// leaves a restore point, so it is recovered instead of discarded.
func (l *Library) Publish(ctx context.Context, staged *objectstore.StagedSnapshot, row model.Backup) error {
	if _, err := l.objects.Publish(staged); err != nil {
		recovered, settleErr := l.settle(ctx, row)
		if recovered && settleErr == nil {
			slog.WarnContext(ctx, "backup: publication recovered after error", "snapshot_id", row.ID, "error", err)
			return nil
		}
		return errors.Join(err, settleErr)
	}
	if err := l.recordSnapshot(ctx, row); err != nil {
		if _, getErr := l.catalog.Backup.Get(ctx, row.ID); getErr != nil {
			// The published manifest is the durable commit: its objects are never
			// discarded because SQLite failed, and startup rebuilds the catalog.
			return fmt.Errorf("backup was saved, but its catalog entry couldn't be updated: %w", errors.Join(err, getErr))
		}
		slog.WarnContext(ctx, "backup: catalog commit returned an error but is durable", "snapshot_id", row.ID, "error", err)
	}
	l.collectDeferred(ctx, row.SourceUDID)
	return nil
}

// collectDeferred measures the source a publication grew or a discard left,
// marks what it finds damaged and drops what no restore point needs. A failure
// is left to the next collection, never failing the caller.
func (l *Library) collectDeferred(ctx context.Context, source string) {
	if err := l.Collect(context.WithoutCancel(ctx), source); err != nil {
		slog.WarnContext(ctx, "object collection deferred", "source", source, "error", err)
	}
}

// Discard drops an unpublished snapshot; what it pooled goes with the next
// collection if this one fails.
func (l *Library) Discard(ctx context.Context, source, id string) error {
	if err := l.objects.Discard(source, id); err != nil {
		return err
	}
	l.collectDeferred(ctx, source)
	return nil
}

// settle resolves an interrupted snapshot: a published one is recovered with
// row's run facts, anything else is discarded. True only on a complete
// recovery.
func (l *Library) settle(ctx context.Context, row model.Backup) (bool, error) {
	published, err := l.objects.Recover(row.SourceUDID, row.ID)
	if err != nil {
		return false, fmt.Errorf("recover interrupted snapshot: %w", err)
	}
	if published == nil {
		l.collectDeferred(ctx, row.SourceUDID)
		return false, nil
	}
	recovered, err := Project(published)
	if err != nil {
		return false, fmt.Errorf("validate interrupted published snapshot: %w", err)
	}
	fctx := context.WithoutCancel(ctx)
	recovered.StartedAt, recovered.TransferredBytes = row.StartedAt, row.TransferredBytes
	if err := l.recordSnapshot(fctx, recovered); err != nil {
		return false, fmt.Errorf("recover published snapshot: %w", err)
	}
	l.collectDeferred(fctx, row.SourceUDID)
	return true, nil
}
