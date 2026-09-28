package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
)

// USBDevice is served as the engine reports it.
type USBDevice = engine.USBDevice

func actionErrorCode(err error, fallback string) string {
	var action *domain.ActionError
	if errors.As(err, &action) && action.Code != "" {
		return action.Code
	}
	return fallback
}

// Already-paired phones are filtered out, but a registered device whose pairing
// broke still shows so it can be re-paired.
func (s *Service) ListPairableUSB(ctx context.Context) ([]USBDevice, error) {
	all, err := s.engine.ListUSBDevices(ctx)
	if err != nil {
		return nil, newEngineActionError("pair_state_failed", err)
	}
	out := make([]USBDevice, 0, len(all))
	for _, d := range all {
		udid := string(d.DeviceID)
		dev, err := s.store.Device.GetByUDID(ctx, udid)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			slog.WarnContext(ctx, "pairing: read device", "udid", udid, "error", err)
			continue
		}
		if err == nil && dev.Paired {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// Success is published only after a fresh lockdown discovery has validated the
// saved record and committed the paired row, so every event-triggered refetch
// observes the new state.
func (s *Service) pairTrustOnce(ctx context.Context, udid string) (engine.PairingOutcome, error) {
	res, err := s.engine.AdvancePairing(ctx, engine.DeviceID(udid))
	if err != nil {
		slog.DebugContext(ctx, "pair trust: engine", "udid", udid, "error", err)
		return res, newEngineActionError("pairing_failed", err)
	}
	if res == engine.TrustWiFiAuthorizationFailed {
		return res, domain.NewActionError(
			"wifi_authorization_failed",
			errors.New("USB pairing succeeded, but Wi-Fi setup could not be completed"),
		)
	}
	if res == engine.TrustPaired {
		if err := s.refreshRegisteredDevices(ctx); err != nil {
			return res, domain.NewActionError("pairing_registration_failed", err)
		}
		device, err := s.store.Device.GetByUDID(ctx, udid)
		if err != nil || !device.Paired {
			return res, fmt.Errorf("pairing succeeded but could not be verified through a new lockdown session")
		}
		s.bus.Emit(pairingChanged(udid, true))
	}
	return res, nil
}

// Contention is refused at admission, not discovered later. Admission runs on
// the request's ctx; the pairing run lives as long as the app.
func (s *Service) StartTrustFlow(ctx context.Context, udid string) (string, error) {
	if domain.ValidateSource(udid) != nil {
		return "", &domain.ValidationError{Code: "invalid_udid", Message: "a valid udid is required"}
	}
	usb, err := s.ListPairableUSB(ctx)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(usb, func(device USBDevice) bool { return string(device.DeviceID) == udid }) {
		return "", domain.ErrDeviceOffline
	}
	return s.launchCommand(s.app, runKindPairing, udid,
		func(ctx context.Context, runID string) error { return s.executeTrustFlow(ctx, runID, udid) },
		deviceWriteResource(udid))
}

func (s *Service) executeTrustFlow(app context.Context, runID, udid string) error {
	ctx, cancel := context.WithTimeout(app, 2*time.Minute)
	defer cancel()
	for {
		status, err := s.pairTrustOnce(ctx, udid)
		if err != nil {
			s.bus.Emit(trustStep(runID, udid, engine.TrustError, actionErrorCode(err, "pairing_failed")))
		} else {
			s.bus.Emit(trustStep(runID, udid, status, ""))
		}
		switch {
		case err != nil:
			return err
		case status == engine.TrustPaired:
			return nil
		case status == engine.TrustDenied:
			return fmt.Errorf("pairing was denied on the phone")
		}
		if !sleepContext(ctx, 1500*time.Millisecond) {
			reason := "pairing timed out — start it again when the phone is ready"
			errorCode := "pairing_timeout"
			if app.Err() != nil {
				reason = "pairing cancelled"
				errorCode = "operation_cancelled"
			}
			s.bus.Emit(trustStep(runID, udid, engine.TrustError, errorCode))
			return fmt.Errorf("%s", reason)
		}
	}
}
