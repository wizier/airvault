package library

import (
	"context"

	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
	"github.com/wizier/airvault/internal/storage"
)

// The catalog's transactional writes. Each pairs a change to the restore points
// with what it does to the source's footprint cache, so the two never disagree.

// The collection that follows a publication measures the footprint.
func (l *Library) recordSnapshot(ctx context.Context, row model.Backup) error {
	return l.catalog.WithTx(ctx, func(tx *storage.Store) error {
		if err := tx.Backup.InsertSnapshot(ctx, row); err != nil {
			return err
		}
		return tx.Backup.ForgetSourceFootprint(ctx, row.SourceUDID)
	})
}

// recordHealth stores what a scan found, each restore point's damage and the
// source's footprint, and returns the restore points whose damage changed.
func (l *Library) recordHealth(ctx context.Context, source string, scan *objectstore.Scan) ([]objectstore.SnapshotHealth, error) {
	var changed []objectstore.SnapshotHealth
	err := l.catalog.WithTx(ctx, func(tx *storage.Store) error {
		for _, snapshot := range scan.Snapshots {
			updated, err := tx.Backup.SetDamage(ctx, source, snapshot.ID, snapshot.Damage, snapshot.DamagedFiles)
			if err != nil {
				return err
			}
			if updated {
				changed = append(changed, snapshot)
			}
		}
		return tx.Backup.SetSourceFootprint(ctx, source, scan.Footprint())
	})
	return changed, err
}

// What this frees depends on which objects the survivors still share, so the
// size is unknown until a collection runs.
func (l *Library) forgetSnapshot(ctx context.Context, source, id string) error {
	return l.catalog.WithTx(ctx, func(tx *storage.Store) error {
		if err := tx.Backup.DeleteSnapshot(ctx, source, id); err != nil {
			return err
		}
		return tx.Backup.ForgetSourceFootprint(ctx, source)
	})
}

// Called once the object store has detached the source's subtree.
func (l *Library) forgetSource(ctx context.Context, source string) error {
	return l.catalog.WithTx(ctx, func(tx *storage.Store) error {
		if err := tx.Backup.DeleteSourceSnapshots(ctx, source); err != nil {
			return err
		}
		return tx.Backup.DropUnreferencedFootprints(ctx)
	})
}

type staleSnapshot struct{ source, id string }

// Stale rows go first so a snapshot that moved sources is re-admitted in the
// same transaction; every changed source loses its cached size.
func (l *Library) replaceCatalog(ctx context.Context, stale []staleSnapshot, admit []model.Backup) error {
	changed := make(map[string]struct{}, len(stale)+len(admit))
	return l.catalog.WithTx(ctx, func(tx *storage.Store) error {
		for _, row := range stale {
			if err := tx.Backup.DeleteSnapshot(ctx, row.source, row.id); err != nil {
				return err
			}
			changed[row.source] = struct{}{}
		}
		for _, row := range admit {
			if err := tx.Backup.InsertSnapshot(ctx, row); err != nil {
				return err
			}
			changed[row.SourceUDID] = struct{}{}
		}
		for source := range changed {
			if err := tx.Backup.ForgetSourceFootprint(ctx, source); err != nil {
				return err
			}
		}
		return tx.Backup.DropUnreferencedFootprints(ctx)
	})
}
