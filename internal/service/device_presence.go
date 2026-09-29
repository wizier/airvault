package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
)

// A lockstate within this window of a lockcomplete is that same lock's trailing
// pulse, not a separate unlock — iOS announces a lock as {lockcomplete, lockstate}.
const screenLockPairWindow = time.Second

// applySnapshot and screenLockSignal commit under deviceTransitionMu, so
// presence, lock signals and their events never reorder.
func (s *Service) applySnapshot(ctx context.Context, items []engine.DevicePresence) bool {
	s.deviceTransitionMu.Lock()
	defer s.deviceTransitionMu.Unlock()

	presence := make(map[string]string, len(items))
	wantRefresh := false
	// pairable = devices the wizard may offer: unknown ones AND registered ones
	// whose pairing is broken — their presence changes emit pair.changed.
	pairable := map[string]bool{}
	for _, item := range items {
		udid := string(item.DeviceID)
		presence[udid] = item.Connection
		dev, err := s.store.Device.GetByUDID(ctx, udid)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			wantRefresh = true
			pairable[udid] = true
		case err != nil:
			slog.Warn("watch: read device", "udid", udid, "error", err)
		case !dev.Paired:
			pairable[udid] = true
		}
	}
	transitions := s.live.applyPresence(presence)
	return s.publishConnections(ctx, transitions, pairable) || wantRefresh
}

func (s *Service) screenLockSignal(udid string, signal engine.ScreenLockSignal) {
	s.deviceTransitionMu.Lock()
	defer s.deviceTransitionMu.Unlock()
	changed, lockScreen, applied := s.live.applyScreenLock(udid, signal, time.Now(), screenLockPairWindow)
	if applied && changed {
		s.bus.Emit(lockScreenChanged(udid, lockScreen))
		s.wakeAutoBackup()
	}
}

func (s *Service) publishConnections(ctx context.Context, transitions []connectionTransition, pairable map[string]bool) bool {
	wantRefresh := false
	for _, transition := range transitions {
		udid, previous, current := transition.udid, transition.from, transition.to
		switch {
		case previous == "" && current != "":
			wantRefresh = true
			slog.Info("device online", "udid", udid, "connection", current)
			s.bus.Emit(deviceOnline(udid, current))
			if pairable[udid] {
				s.bus.Emit(pairableChanged(udid))
			}
		case current == "":
			slog.Info("device offline", "udid", udid)
			dev, err := s.store.Device.GetByUDID(ctx, udid)
			if err != nil || !dev.Paired {
				s.bus.Emit(pairableChanged(udid))
			}
			if err == nil {
				// Write before emit so an event-triggered refetch sees fresh LastSeen.
				if touchErr := s.store.Device.TouchLastSeen(ctx, udid, time.Now().Unix()); touchErr != nil {
					slog.Warn("watch: touch last seen", "udid", udid, "error", touchErr)
				}
			}
			s.bus.Emit(deviceOffline(udid))
		default:
			s.bus.Emit(connectionChanged(udid, current))
		}
		// The lock observer lives while a PAIRED device is online over Wi-Fi:
		// an unknown or broken-pairing phone would only feed a lockdown-refusal
		// retry loop. Lock state is Wi-Fi-only anyway.
		if current == "wifi" && !pairable[udid] {
			s.lockObs.setOnline(udid)
		} else {
			s.lockObs.setOffline(udid)
		}
	}
	return wantRefresh
}
