package service

import (
	"context"
	"log/slog"
	"slices"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
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
	if err := s.deleteSource(ctx, udid, release); err != nil {
		return err
	}
	// Wiping a source's history resets its status to "never", stale failure included.
	s.runs.forget(udid)
	return nil
}

// deleteSource deletes a source's restore points, closing those being browsed.
func (s *Service) deleteSource(ctx context.Context, udid string, release func()) error {
	s.unlocked.closeIf(func(b *unlockedBackup) bool { return b.source == udid })
	return s.library.DeleteSource(ctx, udid, release)
}

func (s *Service) DeleteSnapshots(ctx context.Context, udid string, snapshotIDs []string) error {
	release, err := s.acquireFor(ctx, "snapshot deletion", udid, snapshotWriteResource(udid))
	if err != nil {
		return err
	}
	s.unlocked.closeIf(func(b *unlockedBackup) bool { return slices.Contains(snapshotIDs, b.snapshotID) })
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
	return s.library.Export(ctx, snapshotID, func(source string, err error) {
		s.noticeReadDamage(ctx, source, err)
	})
}

// StartVerify checks that every object the source's restore points need still
// hashes to its name. The phone need not be connected; the source's backups
// wait until the check ends.
func (s *Service) StartVerify(ctx context.Context, udid string) (string, error) {
	if domain.ValidateSource(udid) != nil {
		return "", &domain.ValidationError{Code: "invalid_udid", Message: "a valid udid is required"}
	}
	run, err := s.reserveRun(runKindVerify, udid, snapshotWriteResource(udid))
	if err != nil {
		return "", err
	}
	if err := s.runs.announce(run, StageVerifying); err != nil {
		s.discardRun(run)
		return "", err
	}
	s.bus.Emit(runStarted(run, ""))
	return s.launchRun(run, func() (runOutcome, error) {
		sink := s.runs.progressSink(run, "", StageVerifying, 0)
		return runOutcome{}, s.library.Verify(run.ctx, udid, func(done, total int64) {
			sink(engine.Progress{BytesDone: done, Percent: int(done * 100 / max(total, 1))})
		})
	}), nil
}

func (s *Service) catalogChanged(source string) { s.bus.Emit(backupCatalogChanged(source)) }
