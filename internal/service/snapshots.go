package service

import (
	"context"
	"fmt"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/events"
)

// deleteBackupSource unpublishes every restore point of one device at once and
// hands the lease to the background removal that reclaims the tree.
func (s *Service) deleteBackupSource(ctx context.Context, lease *operationLease, source string) error {
	finalCtx := context.WithoutCancel(ctx)
	defer s.reclaimInBackground(finalCtx, lease, source, func() error {
		return s.objects.RemoveSourceTree(source)
	})
	if err := s.objects.UnpublishSource(source); err != nil {
		return err
	}
	if err := s.forgetSource(finalCtx, source); err != nil {
		return err
	}
	s.bus.Emit(events.BackupCatalog, map[string]any{"udid": source})
	return nil
}

// requireSourceSnapshot confirms the catalog snapshot exists and belongs to
// udid; a foreign snapshot id surfaces as ErrNotFound.
func (s *Service) requireSourceSnapshot(ctx context.Context, udid, snapshotID string) error {
	snapshot, err := s.store.Backup.Get(ctx, snapshotID)
	if err != nil {
		return err
	}
	if snapshot.SourceUDID != udid {
		return domain.ErrNotFound
	}
	return nil
}

// DeleteSnapshots removes restore points on user request. Manifests, catalog
// rows and the footprint recount are synchronous; object collection runs in the
// background and keeps every object a surviving manifest still points at.
func (s *Service) DeleteSnapshots(ctx context.Context, udid string, snapshotIDs []string) error {
	if len(snapshotIDs) == 0 {
		return &domain.ValidationError{Code: "snapshot_required", Message: "select at least one restore point"}
	}
	lease, err := s.acquireFor(ctx, "snapshot deletion", udid, snapshotWriteResource(udid))
	if err != nil {
		return err
	}
	// Every path that does not hand the lease to the background sweep gives it
	// back here; a leaked one wedges the source until restart.
	handedOff := false
	defer func() {
		if !handedOff {
			lease.Release()
		}
	}()
	for _, snapshotID := range snapshotIDs {
		if err := s.requireSourceSnapshot(ctx, udid, snapshotID); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finalCtx := context.WithoutCancel(ctx)
	removed := false
	var deleteErr error
	for _, snapshotID := range snapshotIDs {
		if deleteErr = s.objects.RemoveSnapshot(udid, snapshotID); deleteErr != nil {
			deleteErr = fmt.Errorf("remove snapshot manifest: %w", deleteErr)
			break
		}
		// Unlinking the manifest is the commit point: the restore point is gone
		// even if its catalog row survives, so the recount and sweep below must
		// run either way.
		removed = true
		if deleteErr = s.forgetSnapshot(finalCtx, udid, snapshotID); deleteErr != nil {
			break
		}
	}
	if !removed {
		return deleteErr
	}
	// Recount from the surviving manifests before answering, so the dashboard
	// never shows an unknown size. The background sweep then reclaims the space
	// against the very set this recount built.
	live := s.recountSourceFootprint(finalCtx, udid)
	s.bus.Emit(events.BackupCatalog, map[string]any{"udid": udid})
	handedOff = true
	s.reclaimInBackground(finalCtx, lease, udid, func() error {
		// A failed recount left no live set, so the collection reads its own.
		if live == nil {
			return s.collectSource(finalCtx, udid)
		}
		return s.objects.CollectLive(udid, live)
	})
	return deleteErr
}

// SnapshotsReclaimable reports how much disk space deleting the restore points
// together would free — objects no kept snapshot references. Distinct from
// their summed sizes, which count shared, non-reclaimable data too.
func (s *Service) SnapshotsReclaimable(ctx context.Context, udid string, snapshotIDs []string) (int64, error) {
	if len(snapshotIDs) == 0 {
		return 0, &domain.ValidationError{Code: "snapshot_required", Message: "select at least one restore point"}
	}
	// Direct acquire: the estimate answers the user at once, so a refusal needs
	// no operation event of its own.
	lease, err := s.ops.acquire("reclaim estimate", snapshotReadResource(udid))
	if err != nil {
		return 0, err
	}
	defer lease.Release()
	for _, snapshotID := range snapshotIDs {
		if err := s.requireSourceSnapshot(ctx, udid, snapshotID); err != nil {
			return 0, err
		}
	}
	return s.objects.ReclaimableBytes(ctx, udid, snapshotIDs)
}
