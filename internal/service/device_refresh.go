package service

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/model"
)

func (s *Service) requestDeviceRefresh() {
	select {
	case s.deviceRefreshKick <- struct{}{}:
	default:
	}
}

// The expensive pass: it reads lockdown metadata from every device.
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
		if getErr != nil && !errors.Is(getErr, domain.ErrNotFound) {
			// Treating it as unknown would overwrite the stored row with a blank one.
			slog.Warn("refresh: read device", "udid", udid, "error", getErr)
			continue
		}
		known := getErr == nil
		device := mergeDiscovered(existing, discovered, now)
		if device == nil {
			continue // not ours (yet) — the pairing flow registers it
		}
		if err := s.store.Device.Upsert(ctx, device); err != nil {
			slog.Warn("refresh: upsert device", "udid", udid, "error", err)
			continue
		}
		if known && existing.Paired && !device.Paired {
			// A reset/revoke while attached has no presence transition for the wizard.
			s.bus.Emit(pairingChanged(udid, false))
		}
		activationChanged := s.live.applyActivation(udid, discovered.ActivationState)
		if !known {
			s.bus.Emit(deviceAdded(udid))
			continue
		}
		if existing.Name != device.Name || existing.ProductType != device.ProductType ||
			existing.IOSVersion != device.IOSVersion || existing.Paired != device.Paired ||
			existing.Encrypted != device.Encrypted || activationChanged {
			s.bus.Emit(deviceUpdated(udid))
		}
	}
	return nil
}

// Only fields proven by this lockdown pass apply: transient probe failures keep
// prior identity and pairing; a definitive unpaired verdict clears trust flags.
// Nil means an unregistered device that is not paired.
func mergeDiscovered(existing *model.Device, discovered engine.DeviceInfo, now int64) *model.Device {
	if existing == nil && discovered.PairingState != engine.PairingStatePaired {
		return nil
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
	return &device
}
