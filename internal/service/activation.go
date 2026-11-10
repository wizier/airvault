package service

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/wizier/airvault/internal/activation"
	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
)

func (s *Service) activateIfNeeded(run *runReservation, deviceName string) (domain.Code, error) {
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
	s.runs.setStage(run, StageActivating)
	defer s.runs.setStage(run, StageRestoring)

	blob, err := s.engine.ActivationSessionInfo(ctx, udid)
	if err != nil {
		return domain.CodeActivationFailed, fmt.Errorf("activation: session info: %w", err)
	}
	handshake, err := activation.Handshake(ctx, blob)
	if err != nil {
		return domain.CodeActivationFailed, err
	}
	info, err := s.engine.ActivationInfo(ctx, udid, handshake)
	if err != nil {
		return domain.CodeActivationFailed, fmt.Errorf("activation: activation info: %w", err)
	}
	record, headers, err := activation.RequestRecord(ctx, info)
	switch {
	case errors.Is(err, activation.ErrActivationLock):
		return domain.CodeActivationLock, err
	case err != nil:
		return domain.CodeActivationFailed, err
	case record == nil:
		slog.InfoContext(ctx, "restore: Apple reports the phone as already activated", "device", deviceName, "udid", run.udid)
		return "", nil
	}
	if err := s.engine.ActivationFinish(ctx, udid, record, headers); err != nil {
		return domain.CodeActivationFailed, fmt.Errorf("activation: applying the record: %w", err)
	}
	slog.InfoContext(ctx, "restore: phone activated", "device", deviceName, "udid", run.udid)
	return "", nil
}
