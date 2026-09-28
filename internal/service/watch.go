package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/engine"
)

// StartWatch follows the muxer: its presence drives device state and kicks
// the metadata refresh.
func (s *Service) StartWatch(ctx context.Context) {
	s.wg.Go(func() { s.runAutoBackupTrigger(ctx, s.fireAutoBackup) })
	// The refresh worker owns every metadata pass, so lockdown discovers never
	// overlap.
	s.wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.deviceRefreshKick:
			}
			if err := s.refreshRegisteredDevices(ctx); err != nil {
				slog.Warn("watch: refresh", "error", err)
			}
		}
	})
	s.wg.Go(func() { s.followPresence(ctx) })
}

// followPresence applies every muxer state; the watcher reconnects on its
// own, so only shutdown ends it.
func (s *Service) followPresence(ctx context.Context) {
	watcher := s.engine.WatchPresence(ctx)
	defer watcher.Close()
	for {
		state, err := watcher.Next()
		if err != nil {
			return
		}
		wantRefresh := s.applySnapshot(ctx, state.Devices)
		// After the snapshot, so event-triggered readers never see the old one.
		s.observeMuxer(state)
		if wantRefresh {
			s.requestDeviceRefresh()
		}
	}
}

// observeMuxer is the only writer of the muxer status.
func (s *Service) observeMuxer(state engine.PresenceState) {
	status, changed, flipped := s.live.applyMuxer(state)
	switch {
	case flipped && status.Up:
		slog.Info("muxer: up", "usb", status.USB, "wifi", status.WiFi)
	case flipped:
		slog.Warn("muxer: down", "error", status.Error)
	}
	if changed {
		s.bus.Emit(muxerChanged(status))
	}
}

func (s *Service) Muxer() MuxerStatus { return s.live.muxerStatus() }

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
