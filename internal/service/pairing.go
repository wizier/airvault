package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
)

func actionErrorCode(err error, fallback string) string {
	var action *domain.ActionError
	if errors.As(err, &action) && action.Code != "" {
		return action.Code
	}
	return fallback
}

// ListPairableUSB lists USB devices that are candidates for pairing: already-paired
// ones are filtered out (the wizard only offers new phones), but a registered
// device whose pairing broke still shows so it can be re-paired.
func (s *Service) ListPairableUSB(ctx context.Context) ([]engine.USBDevice, error) {
	all, err := s.engine.ListUSBDevices(ctx)
	if err != nil {
		return nil, newEngineActionError("pair_state_failed", err)
	}
	out := make([]engine.USBDevice, 0, len(all))
	for _, d := range all {
		udid := string(d.DeviceID)
		if dev, gerr := s.store.Device.GetByUDID(ctx, udid); gerr == nil && dev.Paired {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// pairTrustOnce runs one pairing attempt. Success is not published until a
// fresh lockdown discovery has validated the saved record and committed the
// paired device row, so every event-triggered refetch observes the new state.
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
		s.bus.Emit(events.PairChanged, map[string]any{"udid": udid, "paired": true})
	}
	return res, nil
}

// StartTrustFlow reserves the device before returning 202, then supervises one
// runtime pairing run: contention is refused at admission, not discovered later.
func (s *Service) StartTrustFlow(udid string) (string, error) {
	ctx := s.app
	if domain.ValidateSource(udid) != nil {
		return "", &domain.ValidationError{Code: "invalid_udid", Message: "a valid udid is required"}
	}
	usb, err := s.ListPairableUSB(ctx)
	if err != nil {
		return "", err
	}
	found := false
	for _, device := range usb {
		if string(device.DeviceID) == udid {
			found = true
			break
		}
	}
	if !found {
		return "", domain.ErrDeviceOffline
	}
	return s.launchCommand(ctx, runKindPairing, udid,
		func(ctx context.Context, runID string) error { return s.executeTrustFlow(ctx, runID, udid) },
		deviceWriteResource(udid))
}

func (s *Service) executeTrustFlow(app context.Context, runID, udid string) error {
	ctx, cancel := context.WithTimeout(app, 2*time.Minute)
	defer cancel()
	for {
		status, err := s.pairTrustOnce(ctx, udid)
		data := map[string]any{"runId": runID, "udid": udid, "status": status}
		if err != nil {
			data["status"] = engine.TrustError
			data["errorCode"] = actionErrorCode(err, "pairing_failed")
		}
		s.bus.Emit(events.PairTrust, data)
		switch {
		case err != nil:
			return err
		case status == engine.TrustPaired:
			return nil
		case status == engine.TrustDenied:
			return fmt.Errorf("pairing was denied on the phone")
		}
		select {
		case <-ctx.Done():
			reason := "pairing timed out — start it again when the phone is ready"
			errorCode := "pairing_timeout"
			if app.Err() != nil {
				reason = "pairing cancelled"
				errorCode = "operation_cancelled"
			}
			s.bus.Emit(events.PairTrust, map[string]any{"runId": runID, "udid": udid, "status": engine.TrustError,
				"errorCode": errorCode})
			return fmt.Errorf("%s", reason)
		case <-time.After(1500 * time.Millisecond):
		}
	}
}
