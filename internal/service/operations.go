package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
	"uuid"

	"github.com/wizier/airvault/internal/domain"
	airlog "github.com/wizier/airvault/internal/logging"
)

type runKind string

const (
	runKindBackup    runKind = "backup"
	runKindRestore   runKind = "restore"
	runKindInstall   runKind = "install"
	runKindUninstall runKind = "uninstall"
	runKindPairing   runKind = "pairing"
	runKindPassword  runKind = "password"
	runKindPower     runKind = "power"
	runKindVerify    runKind = "verify"
	runKindErase     runKind = "erase"
)

type runState string

const (
	runStateRunning   runState = "running"
	runStateCompleted runState = "completed"
	runStateFailed    runState = "failed"
	runStateCancelled runState = "cancelled"
	runStateRejected  runState = "rejected"
)

type runReservation struct {
	id      string
	kind    runKind
	udid    string
	auto    bool // started by the automatic-backup trigger
	ctx     context.Context
	cancel  context.CancelFunc
	release func()
	started time.Time
}

func (r *runReservation) logAttrs() []slog.Attr {
	if r.auto {
		return []slog.Attr{slog.Bool("auto", true)}
	}
	return nil
}

// A refusal is logged at Info so no rejection loses the holder it names. A site
// that wants another level, or none, acquires directly and says why.
func (s *Service) acquireFor(ctx context.Context, kind runKind, udid string,
	requests ...resourceRequest) (func(), error) {
	release, err := s.ops.acquire(string(kind), requests...)
	if err != nil {
		return nil, rejectOperation(ctx, kind, udid, err)
	}
	return release, nil
}

// The run stays invisible until announceRun, so validation may reject the
// request without a run, terminal event or sticky error. The run lives as long
// as the app, not the request that admitted it.
func (s *Service) reserveRun(kind runKind, udid string, requests ...resourceRequest) (*runReservation, error) {
	id := uuid.New().String()
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

func (s *Service) discardRun(run *runReservation) {
	run.cancel()
	run.release()
}

func rejectOperation(ctx context.Context, kind runKind, udid string, cause error) error {
	slog.InfoContext(ctx, "operation rejected", "event", "operation.rejected",
		"kind", kind, "udid", udid, "state", runStateRejected, "error", cause)
	return cause
}

type commandReservation struct {
	id      string
	kind    runKind
	udid    string
	ctx     context.Context
	release func()
	started time.Time
}

func (s *Service) reserveCommand(ctx context.Context, kind runKind, udid string,
	requests ...resourceRequest) (*commandReservation, error) {
	id := uuid.New().String()
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
		// A verified side effect stays successful even if the client left right
		// after: cancellation cannot undo a command the phone already accepted.
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

// Validation that does not need a lease should happen before this call.
func (s *Service) runCommand(ctx context.Context, kind runKind, udid string,
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

// Completion reaches clients through SSE, never a join.
func (s *Service) launchCommand(ctx context.Context, kind runKind, udid string,
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

func logOperationStarted(ctx context.Context, kind runKind, udid string, extra ...slog.Attr) {
	attrs := []slog.Attr{
		slog.String("event", "operation.started"),
		slog.String("kind", string(kind)),
		slog.String("udid", udid),
		slog.String("state", string(runStateRunning)),
	}
	slog.LogAttrs(ctx, slog.LevelInfo, "operation started", append(attrs, extra...)...)
}

func logOperationFinished(
	ctx context.Context,
	kind runKind,
	udid string,
	state runState,
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
		slog.String("event", "operation."+string(state)),
		slog.String("kind", string(kind)),
		slog.String("udid", udid),
		slog.String("state", string(state)),
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
