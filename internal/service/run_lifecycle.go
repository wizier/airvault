package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
)

// RunStage is the machine name of a run's phase; the UI owns its label.
type RunStage string

const (
	StageWaiting           RunStage = "waiting_for_device"
	StagePreparing         RunStage = "preparing"
	StageActivating        RunStage = "activating"
	StageBackingUp         RunStage = "backing_up"
	StageCalculating       RunStage = "calculating_changes"
	StageFinalizing        RunStage = "finalizing"
	StageRestoring         RunStage = "restoring"
	StageCancellingBackup  RunStage = "cancelling_backup"
	StageCancellingRestore RunStage = "cancelling_restore"
	StageVerifying         RunStage = "verifying"
	StageCancellingVerify  RunStage = "cancelling_verify"
)

type runOutcome struct {
	errorCode string
	sizeBytes int64
}

// completeRun alone classifies a run: a failure after a user cancel from the UI
// or the phone (not shutdown) is cancelled. The run is removed before the
// terminal SSE, so no later progress frame can overtake it.
func (s *Service) completeRun(run *runReservation, outcome runOutcome, runErr error) {
	state := runStateCompleted
	eventErrorCode := outcome.errorCode
	switch {
	case runErr == nil:
		// A verified success: a cancel arriving just after cannot turn a published
		// snapshot or completed restore into a cancelled run.
	case s.app.Err() == nil && (run.ctx.Err() != nil || engineErrorCode(runErr) == "operation_cancelled"):
		state = runStateCancelled
		eventErrorCode = "operation_cancelled"
	default:
		state = runStateFailed
		if eventErrorCode == "" {
			eventErrorCode = run.kind + "_failed"
		}
	}

	// No progress may follow a terminal event.
	s.runs.hide(run, state, eventErrorCode)
	extra := run.logAttrs()
	if outcome.sizeBytes > 0 {
		extra = append(extra, slog.Int64("size_bytes", outcome.sizeBytes))
	}
	if eventErrorCode != "" {
		extra = append(extra, slog.String("error_code", eventErrorCode))
	}
	logOperationFinished(context.WithoutCancel(run.ctx), run.kind, run.udid, state,
		run.started, runErr, extra...)
	var sizeBytes int64
	if state == runStateCompleted {
		sizeBytes = outcome.sizeBytes
	}
	s.bus.Emit(runEnded(run, state, eventErrorCode, sizeBytes))
}

func (s *Service) launchRun(run *runReservation, execute func() (runOutcome, error)) string {
	s.wg.Go(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(run.ctx, "operation worker panicked", "kind", run.kind,
					"panic", recovered, "stack", string(debug.Stack()))
				// The panic unwound past the run's own cleanup while its snapshot
				// write lease is still held, so every staging envelope for the source
				// is this run's. A stranded one disables object collection.
				if run.kind == runKindBackup {
					if err := s.library.ReconcileStaging(run.udid); err != nil {
						slog.ErrorContext(run.ctx, "staging cleanup after panic failed",
							"source", run.udid, "error", err)
					}
				}
				s.completeRun(run, runOutcome{}, fmt.Errorf("%s worker panicked", run.kind))
			}
			run.cancel()
			run.release()
		}()
		outcome, err := execute()
		s.completeRun(run, outcome, err)
	})
	return run.id
}

// engineErrorCode maps an engine error to a transfer's public code, "" for none.
func engineErrorCode(err error) string {
	engineErr, ok := errors.AsType[*engine.Error](err)
	if !ok {
		return ""
	}
	switch engineErr.Kind {
	case engine.ErrorInvalidBackupPassword:
		return "invalid_backup_password"
	case engine.ErrorStorageFull:
		return "storage_full"
	case engine.ErrorDeviceStorageFull:
		return "device_storage_full"
	case engine.ErrorBusy:
		return "resource_busy"
	case engine.ErrorProtocol, engine.ErrorConnectionLost:
		return "device_connection_interrupted"
	case engine.ErrorTimeout:
		return "device_timeout"
	case engine.ErrorDeviceUnavailable:
		return "device_offline"
	case engine.ErrorTrustRequired, engine.ErrorUserDenied:
		return "pairing_required"
	case engine.ErrorDeviceLocked:
		return "device_locked"
	case engine.ErrorFindMyEnabled:
		return "find_my_enabled"
	case engine.ErrorBackupNotConfirmed:
		return errorBackupNotConfirmed
	case engine.ErrorIntegrity:
		return "backup_integrity_failed"
	case engine.ErrorCancelled:
		return "operation_cancelled"
	case engine.ErrorOutcomeUnknown:
		return "operation_outcome_unknown"
	default:
		return ""
	}
}

func newTransferActionError(operationCode string, err error) *domain.ActionError {
	return domain.NewActionError(cmp.Or(engineErrorCode(err), operationCode), err)
}

// newEngineActionError is for device requests: a missing item is a 404, and
// protocol or integrity failures keep the operation's own code.
func newEngineActionError(operationCode string, err error) error {
	if engineErr, ok := errors.AsType[*engine.Error](err); ok {
		switch engineErr.Kind {
		case engine.ErrorNotFound:
			return fmt.Errorf("%w: %w", domain.ErrNotFound, err)
		case engine.ErrorProtocol, engine.ErrorIntegrity:
			return domain.NewActionError(operationCode, err)
		}
	}
	return newTransferActionError(operationCode, err)
}
