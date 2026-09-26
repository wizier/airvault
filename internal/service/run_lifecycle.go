package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
)

// progressSink builds the engine-progress callback for backup/restore. Speed is
// the average over the interval each emit covers, `total` makes the percentage an
// exact byte ratio, and `idleStage` names the wait before the first payload byte.
func (s *Service) progressSink(run *runReservation, idleStage, activeStage RunStage, total int64) func(engine.Progress) {
	const emitEvery = 5 * time.Second
	var (
		lastEmit    time.Time
		emitted     int64
		transferred int64
		speed       int64
	)
	return func(p engine.Progress) {
		finalizing := p.Phase == engine.ProgressPhaseSealing
		transferred = p.BytesDone // the engine keeps it monotonic
		percent := p.Percent
		if total > 0 {
			percent = min(int(transferred*100/total), 100)
		}
		progress := RunProgress{RunID: run.id, UDID: run.udid, Restore: run.kind == runKindRestore,
			Auto: run.auto, Percent: percent, Transferred: transferred}
		emit := finalizing || time.Since(lastEmit) >= emitEvery
		if emit && !lastEmit.IsZero() {
			speed = int64(float64(transferred-emitted) / time.Since(lastEmit).Seconds())
		}
		progress.Speed = speed
		s.runMu.Lock()
		active := s.runs[run.udid]
		if finalizing && active.phase == runPhaseActive {
			active.phase = runPhaseFinalizing
		}
		switch active.phase {
		case runPhaseFinalizing:
			progress.Stage, progress.Percent, progress.Speed = StageFinalizing, 100, 0
		case runPhaseCancelling:
			progress.Stage, progress.Cancelling, progress.Speed = active.progress.Stage, true, 0
		default:
			progress.Stage = activeStage
			if transferred == 0 && percent == 0 && idleStage != "" {
				progress.Stage = idleStage
			}
		}
		active.progress = progress
		// Keep progress ordered with cancellation and terminal removal.
		if emit {
			emitted, lastEmit = transferred, time.Now()
			s.bus.Emit(events.BackupProgress, progress)
		}
		s.runMu.Unlock()
	}
}

func (s *Service) transferredBytes(run *runReservation) int64 {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if active := s.runs[run.udid]; active != nil {
		return active.progress.Transferred
	}
	return 0
}

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
)

// setRunStage publishes a coarse Go-side stage (e.g. activation) before the
// engine's own progress stream takes over.
func (s *Service) setRunStage(run *runReservation, stage RunStage) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	active := s.runs[run.udid]
	if active == nil || active.phase != runPhaseActive {
		return
	}
	active.progress.Stage = stage
	s.bus.Emit(events.BackupProgress, active.progress)
}

type runOutcome struct {
	errorCode string
	sizeBytes int64
}

// beginCommit is the cancellation boundary: before it a run may be discarded;
// after it the verified result must reach durable storage.
func (s *Service) beginCommit(run *runReservation) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	active := s.runs[run.udid]
	if run.ctx.Err() != nil || active.phase == runPhaseCancelling {
		return false
	}
	active.phase = runPhaseCommitting
	return true
}

// completeRun alone classifies a run: a failure after a user cancel (not
// shutdown) is cancelled, whether the cancel came from the UI or from the
// phone, which the engine reports as a cancelled transfer. It removes the
// runtime run before publishing the terminal SSE, so no later progress frame
// can overtake the terminal event.
func (s *Service) completeRun(run *runReservation, outcome runOutcome, runErr error) {
	state := runStateCompleted
	eventErrorCode := outcome.errorCode
	switch {
	case runErr == nil:
		// The engine and publication path returned a verified success. A cancel
		// arriving just after that point cannot retroactively turn an immutable
		// published snapshot (or completed restore) into a cancelled run.
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
	s.hideRun(run, state, eventErrorCode)
	extra := run.logAttrs()
	if outcome.sizeBytes > 0 {
		extra = append(extra, slog.Int64("size_bytes", outcome.sizeBytes))
	}
	if eventErrorCode != "" {
		extra = append(extra, slog.String("error_code", eventErrorCode))
	}
	logOperationFinished(context.WithoutCancel(run.ctx), run.kind, run.udid, state,
		run.started, runErr, extra...)
	data := map[string]any{
		"runId": run.id,
		"udid":  run.udid,
		"state": state,
	}
	if run.kind == runKindRestore {
		data["restore"] = true
	}
	if run.auto {
		data["auto"] = true
	}
	if eventErrorCode != "" {
		data["errorCode"] = eventErrorCode
	}
	switch state {
	case runStateCompleted:
		if outcome.sizeBytes > 0 {
			data["sizeBytes"] = outcome.sizeBytes
		}
		s.bus.Emit(events.BackupDone, data)
	case runStateCancelled:
		s.bus.Emit(events.BackupCancelled, data)
	default:
		s.bus.Emit(events.BackupFailed, data)
	}
}

// launchRun supervises one reserved run and returns its id; completion reaches
// clients through the terminal SSE event, never a join.
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
					if err := s.objects.ReconcileSourceStaging(run.udid); err != nil {
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

// engineErrorCode maps only the engine's stable typed classification to the
// HTTP/SSE contract; diagnostic text is never inspected.
func engineErrorCode(err error) string {
	var engineErr *engine.Error
	if !errors.As(err, &engineErr) {
		return ""
	}
	switch engineErr.Kind {
	case engine.ErrorInvalidBackupPassword:
		return "invalid_backup_password"
	case engine.ErrorStorageFull:
		return "storage_full"
	case engine.ErrorBusy:
		return "resource_busy"
	case engine.ErrorProtocol:
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

func newEngineActionError(operationCode string, err error) *domain.ActionError {
	code := engineErrorCode(err)
	if code == "" {
		code = operationCode
	}
	return domain.NewActionError(code, err)
}
