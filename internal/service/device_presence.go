package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
)

// A lockstate within this window of a lockcomplete is that same lock's trailing
// pulse, not a separate unlock — iOS announces a lock as {lockcomplete, lockstate}.
const screenLockPairWindow = time.Second

// applySnapshot folds raw muxer presence into the low-level runtime store, then
// performs the Service-layer registration/event policy for its transitions.
// deviceTransitionMu keeps state commits and emitted domain effects ordered.
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
		presence[udid] = item.PreferredTransport.String()
		dev, err := s.store.Device.GetByUDID(ctx, udid)
		if err != nil {
			wantRefresh = true
			pairable[udid] = true
		} else if !dev.Paired {
			pairable[udid] = true
		}
	}
	connections := s.live.applyPresence(presence)
	wantRefresh = s.publishConnections(ctx, connections, pairable) || wantRefresh
	return wantRefresh
}

// screenLockSignal resolves SpringBoard's paired notifications into an
// absolute lock state without leaking notification ordering into business code.
func (s *Service) screenLockSignal(udid string, signal engine.ScreenLockSignal) {
	s.deviceTransitionMu.Lock()
	defer s.deviceTransitionMu.Unlock()
	changed, locked, applied := s.live.applyScreenLock(udid, signal, time.Now(), screenLockPairWindow)
	if applied && changed {
		s.bus.Emit(events.DeviceUpdated, map[string]any{"udid": udid, "lockScreen": locked})
	}
}

// publishConnections translates pure runtime transitions into persistence and
// domain events. The runtime store itself knows nothing about either concern.
func (s *Service) publishConnections(ctx context.Context, connections map[string]string, pairable map[string]bool) bool {
	wantRefresh := false
	for _, transition := range s.live.publish(connections) {
		udid, previous, current := transition.udid, transition.from, transition.to
		switch {
		case previous == "" && current != "":
			wantRefresh = true
			slog.Info("device online", "udid", udid, "connection", current)
			s.bus.Emit(events.DeviceOnline, map[string]any{"udid": udid, "connection": current})
			if pairable[udid] {
				s.bus.Emit(events.PairChanged, map[string]any{"udid": udid})
			}
		case current == "":
			slog.Info("device offline", "udid", udid)
			dev, err := s.store.Device.GetByUDID(ctx, udid)
			if err != nil || !dev.Paired {
				s.bus.Emit(events.PairChanged, map[string]any{"udid": udid})
			}
			if err == nil {
				// Write before emit so an event-triggered refetch sees fresh LastSeen.
				if touchErr := s.store.Device.TouchLastSeen(ctx, udid, time.Now().Unix()); touchErr != nil {
					slog.Warn("watch: touch last seen", "udid", udid, "error", touchErr)
				}
			}
			s.bus.Emit(events.DeviceOffline, map[string]any{"udid": udid})
		default:
			s.bus.Emit(events.DeviceUpdated, map[string]any{"udid": udid, "connection": current})
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
