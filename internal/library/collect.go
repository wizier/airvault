package library

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/objectstore"
)

// Collect drops the source's corrupt restore points and every object no
// restore point reaches, then records the source's measured footprint. The
// caller holds the source's write lease.
func (l *Library) Collect(ctx context.Context, source string) error {
	live, corrupt, err := l.objects.ScanLive(ctx, source, nil)
	if err != nil {
		return fmt.Errorf("scan source manifests: %w", err)
	}
	for _, id := range corrupt {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.dropCorrupt(ctx, source, id); err != nil {
			return err
		}
	}
	if err := l.objects.CollectLive(source, live); err != nil {
		return fmt.Errorf("collect source objects: %w", err)
	}
	l.cacheFootprint(ctx, source, live.Footprint())
	l.changed(source)
	return nil
}

func (l *Library) dropCorrupt(ctx context.Context, source, id string) error {
	// Catalog first: if the unlink then fails, the manifest is rediscovered on
	// the next pass and the incomplete live set never reaches the sweep.
	if err := l.forgetSnapshot(context.WithoutCancel(ctx), source, id); err != nil {
		return fmt.Errorf("drop corrupt catalog row: %w", err)
	}
	if err := l.objects.RemoveSnapshot(source, id); err != nil {
		return fmt.Errorf("drop corrupt manifest: %w", err)
	}
	slog.ErrorContext(ctx, "scrub: corrupt restore point removed", "source", source, "snapshot_id", id)
	return nil
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
	// very set this recount built.
	live := l.recountFootprint(finalCtx, source)
	l.changed(source)
	handedOff = true
	l.reclaimInBackground(finalCtx, release, source, func() error {
		// A failed recount left no live set, so the collection reads its own.
		if live == nil {
			return l.Collect(finalCtx, source)
		}
		return l.objects.CollectLive(source, live)
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

// A failed cache write leaves the size stale until the next collection, which
// is never worth failing the caller for.
func (l *Library) cacheFootprint(ctx context.Context, source string, diskBytes int64) {
	if err := l.catalog.Backup.SetSourceFootprint(ctx, source, diskBytes); err != nil {
		slog.WarnContext(ctx, "source usage cache update failed", "source", source, "error", err)
	}
}

// The live set is handed back so the sweep needs no second traversal; nil
// means the read failed and the sweep has to do its own.
func (l *Library) recountFootprint(ctx context.Context, source string) *objectstore.LiveSet {
	live, err := l.objects.LiveObjects(ctx, source, nil)
	if err == nil {
		l.cacheFootprint(ctx, source, live.Footprint())
		return live
	}
	slog.WarnContext(ctx, "snapshot deletion: footprint recount left to collection",
		"source", source, "error", err)
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
