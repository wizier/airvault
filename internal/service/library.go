package service

import (
	"context"
	"log/slog"

	"github.com/wizier/airvault/internal/library"
)

// The service's side of the library: the leases its operations run under and
// the events their changes raise.

type BackupExport = library.Export

// ReconcileBackupStore runs at startup, before any operation, and returns the
// sources for StartMaintenance.
func (s *Service) ReconcileBackupStore(ctx context.Context) ([]string, error) {
	return s.library.Reconcile(ctx)
}

func (s *Service) StartMaintenance(ctx context.Context, sources []string) {
	s.wg.Go(func() { s.scrub(ctx, sources) })
}

// A busy source is left to its owner and a failing one logged; both retry on
// the next startup.
func (s *Service) scrub(ctx context.Context, sources []string) {
	for _, source := range sources {
		if ctx.Err() != nil {
			return
		}
		// Direct acquire: a busy source at startup is expected, so the refusal is
		// Debug here rather than the Info an operation's rejection gets.
		release, err := s.ops.acquire("maintenance", snapshotWriteResource(source))
		if err != nil {
			slog.DebugContext(ctx, "maintenance: source busy, left to its owner",
				"source", source, "error", err)
			continue
		}
		if err := s.library.Collect(ctx, source); err != nil {
			slog.WarnContext(ctx, "maintenance: source deferred", "source", source, "error", err)
		}
		release()
	}
}

// The device row may be absent: the catalog is rebuilt from disk independently.
// No device lease: nothing here touches the phone.
func (s *Service) DeleteBackups(ctx context.Context, udid string) error {
	release, err := s.acquireFor(ctx, "backup deletion", udid, snapshotWriteResource(udid))
	if err != nil {
		return err
	}
	if err := s.library.DeleteSource(ctx, udid, release); err != nil {
		return err
	}
	// Wiping a source's history resets its status to "never", stale failure included.
	s.clearRunOutcomes(udid)
	return nil
}

func (s *Service) DeleteSnapshots(ctx context.Context, udid string, snapshotIDs []string) error {
	release, err := s.acquireFor(ctx, "snapshot deletion", udid, snapshotWriteResource(udid))
	if err != nil {
		return err
	}
	return s.library.DeleteSnapshots(ctx, udid, snapshotIDs, release)
}

func (s *Service) SnapshotsReclaimable(ctx context.Context, udid string, snapshotIDs []string) (int64, error) {
	// Direct acquire: the estimate answers the user at once, so a refusal needs
	// no operation event of its own.
	release, err := s.ops.acquire("reclaim estimate", snapshotReadResource(udid))
	if err != nil {
		return 0, err
	}
	defer release()
	return s.library.Reclaimable(ctx, udid, snapshotIDs)
}

func (s *Service) OpenBackupExport(ctx context.Context, snapshotID string) (*BackupExport, error) {
	return s.library.Export(ctx, snapshotID)
}

func (s *Service) catalogChanged(source string) { s.bus.Emit(backupCatalogChanged(source)) }
