package service

import (
	"context"
	"log/slog"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
)

// Serialized with discovery so a stale metadata pass cannot overwrite the
// outcome committed here.
func (s *Service) ChangeBackupPassword(ctx context.Context, udid, old, new string) error {
	if old == "" && new == "" {
		return &domain.ValidationError{Code: "backup_password_change_empty", Message: "provide the old and/or new password"}
	}
	if err := s.reachableDevice(ctx, udid); err != nil {
		return err
	}
	return s.runCommand(ctx, runKindPassword, udid, func(ctx context.Context) error {
		// Refresh after every request that could have reached iOS, including
		// rejection, cancellation and an indeterminate transport outcome.
		defer s.requestDeviceRefresh()
		result, engineErr := s.engine.ChangeBackupPassword(ctx, engine.DeviceID(udid), old, new)
		// A request can reach the phone after its HTTP client has gone away. Commit
		// any state the engine actually observed using a non-cancelled context.
		if result.EncryptionKnown {
			commitCtx := context.WithoutCancel(ctx)
			s.deviceRefreshMu.Lock()
			err := s.store.Device.SetEncrypted(commitCtx, udid, result.Encrypted)
			s.deviceRefreshMu.Unlock()
			if err != nil {
				if engineErr == nil {
					return err
				}
				slog.WarnContext(commitCtx, "persist backup encryption reconciliation", "udid", udid, "error", err)
			} else {
				s.bus.Emit(deviceUpdated(udid))
			}
		}
		if engineErr != nil {
			slog.DebugContext(ctx, "backup password change rejected", "udid", udid, "error", engineErr)
			return newTransferActionError("backup_password_change_failed", engineErr)
		}
		return nil
	}, deviceWriteResource(udid))
}
