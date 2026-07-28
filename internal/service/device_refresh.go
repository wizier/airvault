package service

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/model"
)

// requestDeviceRefresh coalesces metadata refresh requests into the single
// worker owned by StartWatch.
func (s *Service) requestDeviceRefresh() {
	select {
	case s.deviceRefreshKick <- struct{}{}:
	default:
	}
}

// refreshPresence is cheap and muxer-local: it never opens a phone service.
func (s *Service) refreshPresence(ctx context.Context) (bool, error) {
	items, err := s.engine.ListPresence(ctx)
	if err != nil {
		return false, err
	}
	return s.applySnapshot(ctx, items), nil
}

// refreshRegisteredDevices is the expensive lockdown metadata synchronization.
// It owns registry persistence; raw presence remains in deviceRuntimeStore.
func (s *Service) refreshRegisteredDevices(ctx context.Context) error {
	s.deviceRefreshMu.Lock()
	defer s.deviceRefreshMu.Unlock()

	found, err := s.engine.InspectDevices(ctx)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, discovered := range found {
		udid := string(discovered.DeviceID)
		existing, getErr := s.store.Device.GetByUDID(ctx, udid)
		known := getErr == nil
		device, keep := mergeDiscovered(existing, discovered, now)
		if !keep {
			continue // not ours (yet) — the pairing flow registers it
		}
		if err := s.store.Device.Upsert(ctx, device); err != nil {
			slog.Warn("refresh: upsert device", "udid", udid, "error", err)
			continue
		}
		if known && existing.Paired && !device.Paired {
			// A reset/revoke while attached has no presence transition for the wizard.
			s.bus.Emit(events.PairChanged, map[string]any{"udid": udid, "paired": false})
		}
		activationChanged := s.live.applyActivation(udid, discovered.ActivationState)
		if !known {
			s.bus.Emit(events.DeviceAdded, map[string]any{"udid": udid})
			continue
		}
		if existing.Name != device.Name || existing.ProductType != device.ProductType ||
			existing.IOSVersion != device.IOSVersion || existing.Paired != device.Paired ||
			existing.Encrypted != device.Encrypted || activationChanged {
			s.bus.Emit(events.DeviceUpdated, map[string]any{"udid": udid})
		}
	}
	return nil
}

// mergeDiscovered applies only fields proven by this lockdown pass. Transient
// probe failures preserve prior identity/pairing; a definitive unpaired verdict
// clears trust-dependent flags.
func mergeDiscovered(existing *model.Device, discovered engine.DeviceInfo, now int64) (*model.Device, bool) {
	if existing == nil && discovered.PairingState != engine.PairingStatePaired {
		return nil, false
	}

	udid := string(discovered.DeviceID)
	device := model.Device{UDID: udid, Name: udid}
	if existing != nil {
		device = *existing
	}
	if discovered.MetadataKnown {
		device.Name = cmp.Or(discovered.Name, udid)
		device.ProductType = discovered.ProductType
		device.IOSVersion = discovered.IOSVersion
	}

	switch discovered.PairingState {
	case engine.PairingStatePaired:
		device.Paired = true
		if discovered.FlagsKnown {
			device.Encrypted = discovered.Encrypted
		}
	case engine.PairingStateUnpaired:
		device.Paired = false
		device.Encrypted = false
	case engine.PairingStateUnknown:
		// Preserve the last confirmed pairing and flag values.
	}
	device.LastSeenAt = &now
	return &device, true
}
