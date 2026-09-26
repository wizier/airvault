package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
)

// availabilityReconcileInterval is a coarse, muxer-local drift correction.
// It never opens a phone service; metadata is refreshed only after an actual
// presence/pairing event, and live battery is requested by its consumer.
const availabilityReconcileInterval = 5 * time.Minute

// StartWatch launches the presence workers in the background: a refresh worker
// that coalesces metadata passes, the muxer's event-driven watcher, reopened
// with backoff so a stream failure never leaves only interval reconciliation,
// and the automatic-backup trigger fed by the lock state they observe.
func (s *Service) StartWatch(ctx context.Context) {
	s.wg.Go(func() { s.runAutoBackupTrigger(ctx, s.fireAutoBackup) })
	// The refresh worker owns every active metadata pass. The periodic branch only
	// re-reads the muxer's local device list; it promotes a real presence change
	// into the same coalesced metadata path.
	s.wg.Go(func() {
		ticker := time.NewTicker(availabilityReconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.deviceRefreshKick:
			case <-ticker.C:
				wantRefresh, err := s.refreshPresence(ctx)
				if err != nil {
					slog.Warn("watch: reconcile availability", "error", err)
				} else if wantRefresh {
					s.requestDeviceRefresh()
				}
				continue
			}
			if err := s.refreshRegisteredDevices(ctx); err != nil {
				slog.Warn("watch: refresh", "error", err)
			}
		}
	})
	s.wg.Go(func() {
		// Seed presence before opening the stream so the initial snapshot orders
		// ahead of the watcher's and can never be overwritten by a stale one applied
		// concurrently.
		if wantRefresh, err := s.refreshPresence(ctx); err != nil {
			slog.Warn("watch: initial availability", "error", err)
		} else if wantRefresh {
			s.requestDeviceRefresh()
		}
		backoff := time.Second
		const maxBackoff = 30 * time.Second
		for ctx.Err() == nil {
			watcher, err := s.engine.OpenPresenceWatcher(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.Warn("watch: presence stream unavailable", "error", err, "retry", backoff)
				}
			} else {
				if s.consumePresence(ctx, watcher) {
					backoff = time.Second
				}
				_ = watcher.Close()
			}
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
		}
	})
}

// consumePresence applies snapshots until the stream ends; true means at least
// one snapshot arrived, so the reopen backoff resets.
func (s *Service) consumePresence(ctx context.Context, watcher *engine.PresenceWatcher) bool {
	muxUp := false
	received := false
	for {
		state, err := watcher.Next()
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("watch: presence stream stopped", "error", err)
			}
			return received
		}
		wantRefresh := s.applySnapshot(ctx, state.Devices)
		// The first state is always announced. Emitted only after the complete
		// presence state has been applied, so event-triggered readers never
		// observe the old snapshot.
		if !received || state.MuxUp != muxUp {
			muxUp = state.MuxUp
			slog.Info("muxer: transition", "up", muxUp)
			s.bus.Emit(events.MuxerChanged, map[string]any{"up": muxUp})
		}
		received = true
		if wantRefresh {
			s.requestDeviceRefresh()
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
