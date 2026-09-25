package service

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/wizier/airvault/internal/activation"
	"github.com/wizier/airvault/internal/engine"
)

const (
	errorCodeActivationLock   = "activation_lock"
	errorCodeActivationFailed = "activation_failed"
)

// activateIfNeeded activates a Setup-Assistant phone before a restore: session
// blob → Apple drmHandshake → activation info → deviceActivation → record. An
// activated phone is a no-op; an Apple ID form reply means Activation Lock.
func (s *Service) activateIfNeeded(run *runReservation, deviceName string) (string, error) {
	ctx, udid := run.ctx, engine.DeviceID(run.udid)
	state, err := s.engine.ActivationState(ctx, udid)
	if err != nil {
		// The phone stays the authority; the restore itself surfaces real errors.
		slog.DebugContext(ctx, "restore: activation preflight unavailable", "device", deviceName, "error", err)
		return "", nil
	}
	if state != "Unactivated" {
		return "", nil
	}
	slog.InfoContext(ctx, "restore: phone is unactivated, activating with Apple", "device", deviceName, "udid", run.udid)
	s.setRunStage(run, StageActivating)
	defer s.setRunStage(run, StageRestoring)

	blob, err := s.engine.ActivationSessionInfo(ctx, udid)
	if err != nil {
		return errorCodeActivationFailed, fmt.Errorf("activation: session info: %w", err)
	}
	handshake, err := activation.Handshake(ctx, blob)
	if err != nil {
		return errorCodeActivationFailed, err
	}
	info, err := s.engine.ActivationInfo(ctx, udid, handshake)
	if err != nil {
		return errorCodeActivationFailed, fmt.Errorf("activation: activation info: %w", err)
	}
	record, headers, err := activation.RequestRecord(ctx, info)
	switch {
	case errors.Is(err, activation.ErrActivationLock):
		return errorCodeActivationLock, err
	case err != nil:
		return errorCodeActivationFailed, err
	case record == nil:
		slog.InfoContext(ctx, "restore: Apple reports the phone as already activated", "device", deviceName, "udid", run.udid)
		return "", nil
	}
	if err := s.engine.ActivationFinish(ctx, udid, record, headers); err != nil {
		return errorCodeActivationFailed, fmt.Errorf("activation: applying the record: %w", err)
	}
	slog.InfoContext(ctx, "restore: phone activated", "device", deviceName, "udid", run.udid)
	return "", nil
}
