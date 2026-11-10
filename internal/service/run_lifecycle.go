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
	errorCode domain.Code
	sizeBytes int64
}

// What a run that failed without a code of its own reports.
var runFailedCodes = map[runKind]domain.Code{
	runKindBackup:  domain.CodeBackupFailed,
	runKindRestore: domain.CodeRestoreFailed,
	runKindVerify:  domain.CodeVerifyFailed,
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
	case s.app.Err() == nil && (run.ctx.Err() != nil || isEngineError(runErr, engine.ErrorCancelled)):
		state = runStateCancelled
		eventErrorCode = domain.CodeOperationCancelled
	default:
		state = runStateFailed
		if eventErrorCode == "" {
			eventErrorCode = runFailedCodes[run.kind]
		}
	}

	// No progress may follow a terminal event.
	s.runs.hide(run, state, eventErrorCode)
	extra := run.logAttrs()
	if outcome.sizeBytes > 0 {
		extra = append(extra, slog.Int64("size_bytes", outcome.sizeBytes))
	}
	if eventErrorCode != "" {
		extra = append(extra, slog.String("error_code", string(eventErrorCode)))
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

func isEngineError(err error, kind engine.ErrorKind) bool {
	engineErr, ok := errors.AsType[*engine.Error](err)
	return ok && engineErr.Kind == kind
}

// engineErrorCode maps an engine error to a transfer's public code, "" for none.
func engineErrorCode(err error) domain.Code {
	engineErr, ok := errors.AsType[*engine.Error](err)
	if !ok {
		return ""
	}
	switch engineErr.Kind {
	case engine.ErrorInvalidBackupPassword:
		return domain.CodeInvalidBackupPassword
	case engine.ErrorStorageFull:
		return domain.CodeStorageFull
	case engine.ErrorDeviceStorageFull:
		return domain.CodeDeviceStorageFull
	case engine.ErrorBusy:
		return domain.CodeResourceBusy
	case engine.ErrorProtocol, engine.ErrorConnectionLost:
		return domain.CodeDeviceConnectionInterrupted
	case engine.ErrorTimeout:
		return domain.CodeDeviceTimeout
	case engine.ErrorDeviceUnavailable:
		return domain.CodeDeviceOffline
	case engine.ErrorTrustRequired, engine.ErrorUserDenied:
		return domain.CodePairingRequired
	case engine.ErrorDeviceLocked:
		return domain.CodeDeviceLocked
	case engine.ErrorFindMyEnabled:
		return domain.CodeFindMyEnabled
	case engine.ErrorBackupNotConfirmed:
		return domain.CodeBackupNotConfirmed
	case engine.ErrorIntegrity:
		return domain.CodeBackupIntegrityFailed
	case engine.ErrorCancelled:
		return domain.CodeOperationCancelled
	case engine.ErrorOutcomeUnknown:
		return domain.CodeOperationOutcomeUnknown
	default:
		return ""
	}
}

func newTransferActionError(operationCode domain.Code, err error) *domain.ActionError {
	return domain.NewActionError(cmp.Or(engineErrorCode(err), operationCode), err)
}

// newEngineActionError is for device requests: a missing item is a 404, and
// protocol or integrity failures keep the operation's own code.
func newEngineActionError(operationCode domain.Code, err error) error {
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
