package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/wizier/airvault/internal/domain"
	airlog "github.com/wizier/airvault/internal/logging"

	"github.com/google/uuid"
)

// runIdentity keys per-kind, per-device run outcome state.
type runIdentity struct{ udid, kind string }

const (
	runKindBackup    = "backup"
	runKindRestore   = "restore"
	runKindInstall   = "install"
	runKindUninstall = "uninstall"
	runKindPairing   = "pairing"
	runKindPassword  = "password"
	runKindPower     = "power"

	runStateRunning   = "running"
	runStateCompleted = "completed"
	runStateFailed    = "failed"
	runStateCancelled = "cancelled"
	runStateRejected  = "rejected"
)

type runReservation struct {
	id      string
	kind    string
	udid    string
	ctx     context.Context
	cancel  context.CancelFunc
	release func()
	started time.Time
}

// acquireFor takes a named operation's resources and logs a refusal at Info, so
// no rejection loses the holder it names. A site that wants another level, or
// none, acquires directly and says why.
func (s *Service) acquireFor(ctx context.Context, kind, udid string,
	requests ...resourceRequest) (func(), error) {
	release, err := s.ops.acquire(kind, requests...)
	if err != nil {
		return nil, rejectOperation(ctx, kind, udid, err)
	}
	return release, nil
}

// reserveRun acquires the run's resources and nothing else. The run stays
// invisible until announceRun, so validation may reject the request without
// ever producing a run, a terminal event or a sticky error. The run lives as
// long as the app, not the request that admitted it.
func (s *Service) reserveRun(kind, udid string, requests ...resourceRequest) (*runReservation, error) {
	id := uuid.NewString()
	runCtx := airlog.WithJobID(s.app, id)
	release, err := s.acquireFor(runCtx, kind, udid, requests...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(runCtx)
	return &runReservation{
		id: id, kind: kind, udid: udid,
		ctx: ctx, cancel: cancel, release: release, started: time.Now(),
	}, nil
}

// discardRun releases a reservation that was never announced.
func (s *Service) discardRun(run *runReservation) {
	run.cancel()
	run.release()
}

// announceRun publishes the reserved run. From here it is visible to Running(),
// progress and cancellation, and it must reach a terminal event via completeRun.
func (s *Service) announceRun(run *runReservation, stage RunStage) error {
	s.runMu.Lock()
	if _, busy := s.runs[run.udid]; busy {
		s.runMu.Unlock()
		return rejectOperation(run.ctx, run.kind, run.udid,
			fmt.Errorf("%w: a run is already active for this device", domain.ErrBusy))
	}
	s.runs[run.udid] = &activeRun{run: run, progress: RunProgress{
		RunID: run.id, UDID: run.udid, Stage: stage, Restore: run.kind == runKindRestore,
	}}
	s.runMu.Unlock()
	logOperationStarted(run.ctx, run.kind, run.udid)
	return nil
}

func rejectOperation(ctx context.Context, kind, udid string, cause error) error {
	slog.InfoContext(ctx, "operation rejected", "event", "operation.rejected",
		"kind", kind, "udid", udid, "state", runStateRejected, "error", cause)
	return cause
}

type commandReservation struct {
	id      string
	kind    string
	udid    string
	ctx     context.Context
	release func()
	started time.Time
}

func (s *Service) reserveCommand(ctx context.Context, kind, udid string,
	requests ...resourceRequest) (*commandReservation, error) {
	id := uuid.NewString()
	ctx = airlog.WithJobID(ctx, id)
	release, err := s.acquireFor(ctx, kind, udid, requests...)
	if err != nil {
		return nil, err
	}
	command := &commandReservation{
		id: id, kind: kind, udid: udid, ctx: ctx, release: release, started: time.Now(),
	}
	logOperationStarted(ctx, kind, udid)
	return command, nil
}

func (s *Service) finishCommand(command *commandReservation, runErr error) error {
	fctx := context.WithoutCancel(command.ctx)
	state := runStateCompleted
	switch {
	case runErr == nil:
		// A verified side effect stays successful even if the HTTP client went
		// away immediately after it completed; request cancellation cannot undo
		// a command already accepted by the phone.
	case command.ctx.Err() != nil:
		state = runStateCancelled
	default:
		state = runStateFailed
	}
	logOperationFinished(fctx, command.kind, command.udid, state, command.started, runErr)
	if state == runStateCancelled {
		return errors.Join(domain.ErrCancelled, runErr)
	}
	return runErr
}

// runCommand gives synchronous mutations the same atomic admission and
// structured logging as backup/restore runs. Validation that does not need a
// lease should happen before this call.
func (s *Service) runCommand(ctx context.Context, kind, udid string,
	execute func(context.Context) error, requests ...resourceRequest) error {
	command, err := s.reserveCommand(ctx, kind, udid, requests...)
	if err != nil {
		return err
	}
	s.wg.Add(1)
	defer s.wg.Done()
	defer command.release()
	return s.finishCommand(command, execute(command.ctx))
}

// launchCommand is the supervised background counterpart to runCommand. It
// returns the run id; completion reaches clients through SSE, never a join.
func (s *Service) launchCommand(ctx context.Context, kind, udid string,
	execute func(context.Context, string) error, requests ...resourceRequest) (string, error) {
	command, err := s.reserveCommand(ctx, kind, udid, requests...)
	if err != nil {
		return "", err
	}
	s.wg.Go(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(command.ctx, "command worker panicked", "kind", kind,
					"panic", recovered, "stack", string(debug.Stack()))
				_ = s.finishCommand(command, fmt.Errorf("%s worker panicked", kind))
			}
			command.release()
		}()
		_ = s.finishCommand(command, execute(command.ctx, command.id))
	})
	return command.id, nil
}

func logOperationStarted(ctx context.Context, kind, udid string) {
	slog.InfoContext(ctx, "operation started", "event", "operation.started",
		"kind", kind, "udid", udid, "state", runStateRunning)
}

func logOperationFinished(
	ctx context.Context,
	kind, udid, state string,
	started time.Time,
	runErr error,
	extra ...slog.Attr,
) {
	level := slog.LevelInfo
	message := "operation completed"
	switch state {
	case runStateFailed:
		level = slog.LevelWarn
		message = "operation failed"
	case runStateCancelled:
		message = "operation cancelled"
	}
	attrs := []slog.Attr{
		slog.String("event", "operation."+state),
		slog.String("kind", kind),
		slog.String("udid", udid),
		slog.String("state", state),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	}
	if runErr != nil {
		attrs = append(attrs, slog.Any("error", runErr))
		cause := runErr
		for range 16 {
			next := errors.Unwrap(cause)
			if next == nil {
				break
			}
			cause = next
		}
		if cause != runErr && cause.Error() != runErr.Error() {
			attrs = append(attrs, slog.Any("error_cause", cause))
		}
	}
	attrs = append(attrs, extra...)
	slog.LogAttrs(ctx, level, message, attrs...)
}

// hideRun removes the runtime run and records the run's terminal outcome in
// the same critical section, so a refetch can never see them disagree.
func (s *Service) hideRun(run *runReservation, state, errorCode string) {
	s.runMu.Lock()
	delete(s.runs, run.udid)
	key := runIdentity{run.udid, run.kind}
	if state == runStateFailed {
		s.lastRunError[key] = errorCode
	} else {
		delete(s.lastRunError, key)
	}
	s.runMu.Unlock()
}
