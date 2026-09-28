package service

import (
	"context"
	"log/slog"

	"github.com/wizier/airvault/internal/objectstore"
)

// A failed cache write leaves the size stale until the next collection, which is
// never worth failing the caller for.
func (s *Service) cacheSourceFootprint(ctx context.Context, source string, diskBytes int64) {
	if err := s.store.Backup.SetSourceFootprint(ctx, source, diskBytes); err != nil {
		slog.WarnContext(ctx, "source usage cache update failed", "source", source, "error", err)
	}
}

// The live set is handed back so the sweep needs no second traversal; nil means
// the read failed and the sweep has to do its own.
func (s *Service) recountSourceFootprint(ctx context.Context, source string) *objectstore.LiveSet {
	live, err := s.objects.LiveObjects(ctx, source, nil)
	if err == nil {
		s.cacheSourceFootprint(ctx, source, live.Footprint())
		return live
	}
	slog.WarnContext(ctx, "snapshot deletion: footprint recount left to collection",
		"source", source, "error", err)
	return nil
}

// Takes ownership of the write lease. A failure leaves unreachable bytes for the
// next startup pass, never a restore point.
func (s *Service) reclaimInBackground(
	ctx context.Context, release func(), source string, reclaim func() error,
) {
	s.wg.Go(func() {
		defer release()
		if err := reclaim(); err != nil {
			slog.WarnContext(ctx, "deletion: reclaim deferred", "source", source, "error", err)
		}
	})
}
