package service

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
)

// newTestService builds a Service with only the fields the run-lifecycle phase
// machine touches (the event bus and the live-run map).
func newTestService() *Service {
	return &Service{app: context.Background(), bus: events.New(), runs: map[string]*activeRun{},
		lastRunError: map[runIdentity]string{}}
}

// registerRun installs backup run "run-1" on "udid-1" in phase Active, as
// reserveRun would.
func registerRun(s *Service) *runReservation {
	ctx, cancel := context.WithCancel(context.Background())
	run := &runReservation{id: "run-1", udid: "udid-1", kind: runKindBackup,
		ctx: ctx, cancel: cancel}
	s.runs[run.udid] = &activeRun{
		run:      run,
		progress: RunProgress{RunID: run.id, UDID: run.udid, Stage: StageBackingUp},
	}
	return run
}

func TestTerminalEventContainsLocalizableCodeOnly(t *testing.T) {
	s := newTestService()
	_, eventsCh, _, _ := s.bus.Subscribe(0)
	run := registerRun(s)

	s.completeRun(run, runOutcome{}, errors.New("unclassified native failure"))
	event := <-eventsCh
	if event.Type != events.BackupFailed {
		t.Fatalf("event type = %q, want %q", event.Type, events.BackupFailed)
	}
	data := event.Data.(map[string]any)
	if got := data["errorCode"]; got != "backup_failed" {
		t.Fatalf("errorCode = %#v, want backup_failed", got)
	}
	if _, exists := data["error"]; exists {
		t.Fatal("terminal event unexpectedly contains a presentation error string")
	}
}

// TestLastRunErrorLifecycle pins the runtime last-outcome record: a failed
// run stores its code per kind for the device overview, the next success of
// that kind clears it without touching the other kind's record.
func TestLastRunErrorLifecycle(t *testing.T) {
	s := newTestService()
	s.lastRunError[runIdentity{"udid-1", runKindRestore}] = "restore_failed"

	run := registerRun(s)
	s.completeRun(run, runOutcome{errorCode: "device_timeout"}, errors.New("native failure"))
	want := map[string]string{runKindBackup: "device_timeout", runKindRestore: "restore_failed"}
	if got := s.lastRunErrors("udid-1"); !maps.Equal(got, want) {
		t.Fatalf("after failure lastRunErrors = %v, want %v", got, want)
	}

	run = registerRun(s)
	s.completeRun(run, runOutcome{}, nil)
	want = map[string]string{runKindRestore: "restore_failed"}
	if got := s.lastRunErrors("udid-1"); !maps.Equal(got, want) {
		t.Fatalf("after success lastRunErrors = %v, want %v", got, want)
	}
}

// TestBeginCommitWinsRefusesCancel and TestCancelWinsRefusesCommit pin both
// runMu-serialized orderings of the cancellation boundary: whichever of
// beginCommit / CancelRun runs first, the other is refused.
func TestBeginCommitWinsRefusesCancel(t *testing.T) {
	s := newTestService()
	run := registerRun(s)

	if !s.beginCommit(run) {
		t.Fatal("beginCommit on an active run returned false")
	}
	if got := s.runs[run.udid].phase; got != runPhaseCommitting {
		t.Fatalf("phase = %d, want Committing", got)
	}
	if err := s.CancelRun("run-1"); !errors.Is(err, domain.ErrOperationState) {
		t.Fatalf("CancelRun after commit returned %v, want ErrOperationState", err)
	}
	if run.ctx.Err() != nil {
		t.Fatal("committing run must not be cancelled")
	}
}

func TestCancelWinsRefusesCommit(t *testing.T) {
	s := newTestService()
	run := registerRun(s)

	if err := s.CancelRun("run-1"); err != nil {
		t.Fatalf("CancelRun on an active run: %v", err)
	}
	if got := s.runs[run.udid].phase; got != runPhaseCancelling {
		t.Fatalf("phase = %d, want Cancelling", got)
	}
	if run.ctx.Err() == nil {
		t.Fatal("cancelled run ctx must be cancelled")
	}
	if s.beginCommit(run) {
		t.Fatal("beginCommit succeeded after cancel")
	}
}

// TestProgressSinkPhaseTransitions checks the sink's phase-driven presentation:
// a finalizing frame latches Finalizing, and once Cancelling a stray finalizing
// frame cannot revert the display.
func TestProgressSinkPhaseTransitions(t *testing.T) {
	s := newTestService()
	run := registerRun(s)
	sink := s.progressSink(run, "", StageBackingUp, 0)

	sink(engine.Progress{BytesDone: 123})
	if got := s.runs[run.udid]; got.phase != runPhaseActive || got.progress.Stage != StageBackingUp {
		t.Fatalf("normal frame: phase=%d stage=%q", got.phase, got.progress.Stage)
	}

	sink(engine.Progress{Phase: engine.ProgressPhaseSealing})
	if got := s.runs[run.udid]; got.phase != runPhaseFinalizing ||
		got.progress.Stage != StageFinalizing || got.progress.Percent != 100 ||
		got.progress.Transferred != 123 || got.progress.Speed != 0 {
		t.Fatalf("finalizing frame: phase=%d stage=%q pct=%d transferred=%d speed=%d",
			got.phase, got.progress.Stage, got.progress.Percent, got.progress.Transferred, got.progress.Speed)
	}

	if err := s.CancelRun("run-1"); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	sink(engine.Progress{Phase: engine.ProgressPhaseSealing})
	if got := s.runs[run.udid]; got.phase != runPhaseCancelling ||
		got.progress.Stage != StageCancellingBackup {
		t.Fatalf("finalizing after cancel: phase=%d stage=%q", got.phase, got.progress.Stage)
	}
}

// The idle stage holds until the phone sends anything: the whole window where
// it diffs against the previous backup. A percentage can arrive before the bytes
// that earned it and means the phone is sending just the same.
func TestProgressSinkLeavesTheIdleStageOnFirstPayload(t *testing.T) {
	for _, test := range []struct {
		name  string
		frame engine.Progress
		want  RunStage
	}{
		{"nothing sent", engine.Progress{}, StageCalculating},
		{"first byte", engine.Progress{BytesDone: 1}, StageBackingUp},
		{"percent alone", engine.Progress{Percent: 1}, StageBackingUp},
	} {
		s := newTestService()
		run := registerRun(s)
		s.progressSink(run, StageCalculating, StageBackingUp, 0)(test.frame)
		if got := s.runs[run.udid].progress.Stage; got != test.want {
			t.Errorf("%s: stage = %q, want %q", test.name, got, test.want)
		}
	}
}

// A known total makes the percentage an exact byte ratio; the clamp covers
// restore options that leave part of a snapshot unsent.
func TestProgressSinkDerivesPercentFromAKnownTotal(t *testing.T) {
	s := newTestService()
	run := registerRun(s)
	sink := s.progressSink(run, "", StageRestoring, 400)

	sink(engine.Progress{BytesDone: 100, Percent: 77})
	if got := s.runs[run.udid].progress; got.Percent != 25 || got.Transferred != 100 {
		t.Fatalf("quarter sent: pct=%d transferred=%d", got.Percent, got.Transferred)
	}

	sink(engine.Progress{BytesDone: 900, Percent: 77})
	if got := s.runs[run.udid].progress.Percent; got != 100 {
		t.Fatalf("overshoot must clamp: pct=%d", got)
	}
}

func TestEngineErrorCodes(t *testing.T) {
	if got := engineErrorCode(errors.New("device returned an unfamiliar error")); got != "" {
		t.Fatalf("unclassified error code = %q, want empty", got)
	}
	tests := []struct {
		kind engine.ErrorKind
		want string
	}{
		{engine.ErrorStorageFull, "storage_full"},
		{engine.ErrorBusy, "resource_busy"},
		{engine.ErrorProtocol, "device_connection_interrupted"},
		{engine.ErrorTimeout, "device_timeout"},
		{engine.ErrorDeviceUnavailable, "device_offline"},
		{engine.ErrorTrustRequired, "pairing_required"},
		{engine.ErrorDeviceLocked, "device_locked"},
		{engine.ErrorIntegrity, "backup_integrity_failed"},
		{engine.ErrorInvalidBackupPassword, "invalid_backup_password"},
		{engine.ErrorOutcomeUnknown, "operation_outcome_unknown"},
	}
	for _, test := range tests {
		err := &engine.Error{Kind: test.kind, Detail: "native diagnostic"}
		if got := engineErrorCode(err); got != test.want {
			t.Errorf("engineErrorCode(kind=%d) = %q, want %q", test.kind, got, test.want)
		}
	}
}
