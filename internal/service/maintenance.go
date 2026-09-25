package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wizier/airvault/internal/events"
)

// StartMaintenance runs one background recovery pass after startup: collect
// every source once. It never spins or blocks the caller.
func (s *Service) StartMaintenance(ctx context.Context, sources []string) {
	s.wg.Go(func() { s.scrub(ctx, sources) })
}

// scrub collects each source under the write lease live mutations take. A busy
// source is left to its owner and a failing one logged; both retry next startup.
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
		if err := s.collectSource(ctx, source); err != nil {
			slog.WarnContext(ctx, "maintenance: source deferred", "source", source, "error", err)
		}
		release()
	}
}

// collectSource reads a source's manifests to rebuild its live object set, drops
// the corrupt restore points that surface, reclaims dead objects and publishes
// the size of what remains. The caller holds the source write lease.
func (s *Service) collectSource(ctx context.Context, source string) error {
	live, corrupt, err := s.objects.ScanLive(ctx, source, nil)
	if err != nil {
		return fmt.Errorf("scan source manifests: %w", err)
	}
	for _, id := range corrupt {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.dropCorruptSnapshot(ctx, source, id); err != nil {
			return err
		}
	}
	if err := s.objects.CollectLive(source, live); err != nil {
		return fmt.Errorf("collect source objects: %w", err)
	}
	s.cacheSourceFootprint(ctx, source, live.Footprint())
	s.bus.Emit(events.BackupCatalog, map[string]any{"udid": source})
	return nil
}

// dropCorruptSnapshot removes a manifest whose seal no longer verifies and its
// catalog row, so a bit-rotted restore point stops pinning objects.
func (s *Service) dropCorruptSnapshot(ctx context.Context, source, id string) error {
	// Projection first: if the unlink then fails, the manifest is rediscovered on
	// the next pass and the incomplete live set never reaches the sweep.
	if err := s.forgetSnapshot(context.WithoutCancel(ctx), source, id); err != nil {
		return fmt.Errorf("drop corrupt catalog row: %w", err)
	}
	if err := s.objects.RemoveSnapshot(source, id); err != nil {
		return fmt.Errorf("drop corrupt manifest: %w", err)
	}
	slog.ErrorContext(ctx, "scrub: corrupt restore point removed", "source", source, "snapshot_id", id)
	return nil
}
