package library

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/objectstore"
)

// Collect records each restore point's health and the source's footprint,
// then deletes every object no restore point needs. The caller holds the
// source's write lease.
func (l *Library) Collect(ctx context.Context, source string) error {
	scan, err := l.recordScan(ctx, source)
	if err != nil {
		return err
	}
	if err := l.objects.Sweep(source, scan); err != nil {
		return fmt.Errorf("collect source objects: %w", err)
	}
	l.changed(source)
	return nil
}

// recordScan scans the source and records each restore point's health with
// the source's footprint; the scan it returns is ready to sweep. Any lease on
// the source will do.
func (l *Library) recordScan(ctx context.Context, source string) (*objectstore.Scan, error) {
	scan, err := l.objects.Scan(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("scan source: %w", err)
	}
	changed, err := l.recordHealth(ctx, source, scan)
	if err != nil {
		return nil, fmt.Errorf("record restore point health: %w", err)
	}
	for _, snapshot := range changed {
		if snapshot.Damage == "" {
			slog.InfoContext(ctx, "health: restore point whole again", "source", source, "snapshot_id", snapshot.ID)
		} else {
			slog.WarnContext(ctx, "health: restore point damaged", "source", source,
				"snapshot_id", snapshot.ID, "damage", snapshot.Damage, "files", snapshot.DamagedFiles)
		}
	}
	return scan, nil
}

// NoticeDamage rescans the source when err is a read that proved stored data
// damaged: the read set the object aside, the scan marks who needs it. The
// caller holds a lease on the source.
func (l *Library) NoticeDamage(ctx context.Context, source string, err error) {
	if !errors.Is(err, objectstore.ErrIntegrity) && !errors.Is(err, objectstore.ErrManifestCorrupt) {
		return
	}
	if _, err := l.recordScan(context.WithoutCancel(ctx), source); err != nil {
		slog.WarnContext(ctx, "health: rescan left to the next collection", "source", source, "error", err)
		return
	}
	l.changed(source)
}

// Verify hashes every object the source's restore points need and records
// the result, even when stopped early. The caller holds the write lease.
func (l *Library) Verify(ctx context.Context, source string, progress func(done, total int64)) error {
	scan, err := l.objects.Scan(ctx, source)
	if err != nil {
		return fmt.Errorf("scan source: %w", err)
	}
	started := time.Now().Unix()
	setAside, verifyErr := l.objects.Verify(ctx, source, scan, progress)
	if setAside > 0 {
		slog.ErrorContext(ctx, "verify: objects failed their hash", "source", source, "objects", setAside)
	}
	finalCtx := context.WithoutCancel(ctx)
	if verifyErr == nil {
		verifyErr = l.catalog.Backup.MarkVerified(finalCtx, source, started)
	}
	return errors.Join(verifyErr, l.Collect(finalCtx, source))
}

// DeleteSnapshots removes restore points of source. Manifests, catalog rows
// and the footprint recount are done when it returns; collection runs in the
// background and then calls release, the source's write lease.
func (l *Library) DeleteSnapshots(ctx context.Context, source string, ids []string, release func()) error {
	// Every path that does not hand the lease to the background sweep gives it
	// back here; a leaked one wedges the source until restart.
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	if err := l.requireSnapshots(ctx, source, ids); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	finalCtx := context.WithoutCancel(ctx)
	removed := false
	var deleteErr error
	for _, id := range ids {
		if deleteErr = l.objects.RemoveSnapshot(source, id); deleteErr != nil {
			deleteErr = fmt.Errorf("remove snapshot manifest: %w", deleteErr)
			break
		}
		// Unlinking the manifest is the commit point: the restore point is gone
		// even if its catalog row survives, so the recount and sweep below must
		// run either way.
		removed = true
		if deleteErr = l.forgetSnapshot(finalCtx, source, id); deleteErr != nil {
			break
		}
	}
	if !removed {
		return deleteErr
	}
	// Recount from the surviving manifests before answering, so the dashboard
	// never shows an unknown size. The sweep then reclaims the space against the
	// very scan this recount read.
	scan, err := l.recordScan(finalCtx, source)
	if err != nil {
		slog.WarnContext(ctx, "snapshot deletion: footprint recount left to collection",
			"source", source, "error", err)
	}
	l.changed(source)
	handedOff = true
	l.reclaimInBackground(finalCtx, release, source, func() error {
		// A failed recount left no scan, so the collection reads its own.
		if scan == nil {
			return l.Collect(finalCtx, source)
		}
		return l.objects.Sweep(source, scan)
	})
	return deleteErr
}

// DeleteSource removes every restore point of source at once; the tree is
// reclaimed in the background, which then calls release.
func (l *Library) DeleteSource(ctx context.Context, source string, release func()) error {
	finalCtx := context.WithoutCancel(ctx)
	defer l.reclaimInBackground(finalCtx, release, source, func() error {
		return l.objects.RemoveSourceTree(source)
	})
	if err := l.objects.UnpublishSource(source); err != nil {
		return err
	}
	if err := l.forgetSource(finalCtx, source); err != nil {
		return err
	}
	l.changed(source)
	return nil
}

// Reclaimable is what deleting ids would free: unlike their summed sizes, it
// leaves out what other restore points still share. The caller holds the
// source's read lease.
func (l *Library) Reclaimable(ctx context.Context, source string, ids []string) (int64, error) {
	if err := l.requireSnapshots(ctx, source, ids); err != nil {
		return 0, err
	}
	return l.objects.ReclaimableBytes(ctx, source, ids)
}

func (l *Library) requireSnapshots(ctx context.Context, source string, ids []string) error {
	if len(ids) == 0 {
		return &domain.ValidationError{Code: "snapshot_required", Message: "select at least one restore point"}
	}
	for _, id := range ids {
		snapshot, err := l.catalog.Backup.Get(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.SourceUDID != source {
			return domain.ErrNotFound
		}
	}
	return nil
}

// Takes ownership of the write lease. A failure leaves unreachable bytes for
// the next startup pass, never a restore point.
func (l *Library) reclaimInBackground(ctx context.Context, release func(), source string, reclaim func() error) {
	l.wg.Go(func() {
		defer release()
		if err := reclaim(); err != nil {
			slog.WarnContext(ctx, "deletion: reclaim deferred", "source", source, "error", err)
		}
	})
}
