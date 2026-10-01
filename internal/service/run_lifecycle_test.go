package service

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"testing"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
)

func TestTerminalEventContainsLocalizableCodeOnly(t *testing.T) {
	s := newTestService()
	_, eventsCh, _, _ := s.bus.Subscribe(0)
	run := registerRun(s)

	s.completeRun(run, runOutcome{}, errors.New("unclassified failure"))
	event := <-eventsCh
	data, err := json.Marshal(event.Data)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"runId":"run-1","udid":"udid-1","state":"failed","errorCode":"backup_failed"}`; event.Type != "backup.failed" || string(data) != want {
		t.Fatalf("event = %s %s, want backup.failed %s", event.Type, data, want)
	}
}

// A failed run stores its code per kind; the next success of that kind clears
// only that kind's record. Neither a cancel from the phone nor an unanswered
// automatic prompt counts as a failure. Forgetting the device clears them all.
func TestLastRunErrorLifecycle(t *testing.T) {
	s := newTestService()
	s.runs.lastError["udid-1"] = map[runKind]string{runKindRestore: "restore_failed"}

	run := registerRun(s)
	s.completeRun(run, runOutcome{errorCode: "device_timeout"}, errors.New("device failure"))
	want := map[runKind]string{runKindBackup: "device_timeout", runKindRestore: "restore_failed"}
	if got := s.runs.lastErrors("udid-1"); !maps.Equal(got, want) {
		t.Fatalf("after failure lastRunErrors = %v, want %v", got, want)
	}

	run = registerRun(s)
	s.completeRun(run, runOutcome{}, nil)
	want = map[runKind]string{runKindRestore: "restore_failed"}
	if got := s.runs.lastErrors("udid-1"); !maps.Equal(got, want) {
		t.Fatalf("after success lastRunErrors = %v, want %v", got, want)
	}

	run = registerRun(s)
	cancelled := &engine.Error{Kind: engine.ErrorCancelled}
	s.completeRun(run, runOutcome{errorCode: engineErrorCode(cancelled)}, cancelled)
	if got := s.runs.lastErrors("udid-1"); !maps.Equal(got, want) {
		t.Fatalf("after a phone cancel lastRunErrors = %v, want %v", got, want)
	}

	run = registerRun(s)
	run.auto = true
	s.completeRun(run, runOutcome{errorCode: errorBackupNotConfirmed}, errors.New("prompt unanswered"))
	if got := s.runs.lastErrors("udid-1"); !maps.Equal(got, want) {
		t.Fatalf("after an unanswered automatic prompt lastRunErrors = %v, want %v", got, want)
	}

	s.runs.lastError["udid-1"][runKindVerify] = "backup_integrity_failed"
	s.runs.forget("udid-1")
	if got := s.runs.lastErrors("udid-1"); got != nil {
		t.Fatalf("after clearing lastRunErrors = %v, want none of any kind", got)
	}
}

// TestBeginCommitWinsRefusesCancel and TestCancelWinsRefusesCommit pin both
// runMu-serialized orderings of the cancellation boundary: whichever of
// beginCommit / CancelRun runs first, the other is refused.
func TestBeginCommitWinsRefusesCancel(t *testing.T) {
	s := newTestService()
	run := registerRun(s)

	if !s.runs.beginCommit(run) {
		t.Fatal("beginCommit on an active run returned false")
	}
	if got := s.runs.active[run.udid].phase; got != runPhaseCommitting {
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
	if got := s.runs.active[run.udid].phase; got != runPhaseCancelling {
		t.Fatalf("phase = %d, want Cancelling", got)
	}
	if run.ctx.Err() == nil {
		t.Fatal("cancelled run ctx must be cancelled")
	}
	if s.runs.beginCommit(run) {
		t.Fatal("beginCommit succeeded after cancel")
	}
}

// TestProgressSinkPhaseTransitions checks the sink's phase-driven presentation:
// a finalizing frame latches Finalizing, and once Cancelling a stray finalizing
// frame cannot revert the display.
func TestProgressSinkPhaseTransitions(t *testing.T) {
	s := newTestService()
	run := registerRun(s)
	sink := s.runs.progressSink(run, "", StageBackingUp, 0)

	sink(engine.Progress{BytesDone: 123})
	if got := s.runs.active[run.udid]; got.phase != runPhaseActive || got.progress.Stage != StageBackingUp {
		t.Fatalf("normal frame: phase=%d stage=%q", got.phase, got.progress.Stage)
	}

	sink(engine.Progress{Phase: engine.ProgressPhaseSealing, BytesDone: 123})
	if got := s.runs.active[run.udid]; got.phase != runPhaseFinalizing ||
		got.progress.Stage != StageFinalizing || got.progress.Percent != 100 ||
		got.progress.Transferred != 123 || got.progress.Speed != 0 {
		t.Fatalf("finalizing frame: phase=%d stage=%q pct=%d transferred=%d speed=%d",
			got.phase, got.progress.Stage, got.progress.Percent, got.progress.Transferred, got.progress.Speed)
	}

	if err := s.CancelRun("run-1"); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	sink(engine.Progress{Phase: engine.ProgressPhaseSealing})
	if got := s.runs.active[run.udid]; got.phase != runPhaseCancelling ||
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
		s.runs.progressSink(run, StageCalculating, StageBackingUp, 0)(test.frame)
		if got := s.runs.active[run.udid].progress.Stage; got != test.want {
			t.Errorf("%s: stage = %q, want %q", test.name, got, test.want)
		}
	}
}

// A known total makes the percentage an exact byte ratio; the clamp covers
// restore options that leave part of a snapshot unsent.
func TestProgressSinkDerivesPercentFromAKnownTotal(t *testing.T) {
	s := newTestService()
	run := registerRun(s)
	sink := s.runs.progressSink(run, "", StageRestoring, 400)

	sink(engine.Progress{BytesDone: 100, Percent: 77})
	if got := s.runs.active[run.udid].progress; got.Percent != 25 || got.Transferred != 100 {
		t.Fatalf("quarter sent: pct=%d transferred=%d", got.Percent, got.Transferred)
	}

	sink(engine.Progress{BytesDone: 900, Percent: 77})
	if got := s.runs.active[run.udid].progress.Percent; got != 100 {
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
		{engine.ErrorBackupNotConfirmed, "backup_not_confirmed"},
		{engine.ErrorConnectionLost, "device_connection_interrupted"},
		{engine.ErrorDeviceStorageFull, "device_storage_full"},
		{engine.ErrorNotFound, ""},
	}
	for _, test := range tests {
		err := &engine.Error{Kind: test.kind, Detail: "device diagnostic"}
		if got := engineErrorCode(err); got != test.want {
			t.Errorf("engineErrorCode(kind=%d) = %q, want %q", test.kind, got, test.want)
		}
	}
}

// A device request has no transfer to name: a missing item is a plain 404 and
// protocol or integrity failures keep the operation's own code.
func TestEngineRequestErrors(t *testing.T) {
	missing := newEngineActionError("download_failed", &engine.Error{Kind: engine.ErrorNotFound})
	if !errors.Is(missing, domain.ErrNotFound) || !errors.Is(missing, fs.ErrNotExist) {
		t.Fatalf("missing item: %v, want domain.ErrNotFound wrapping fs.ErrNotExist", missing)
	}
	tests := []struct {
		err  error
		want string
	}{
		{&engine.Error{Kind: engine.ErrorProtocol}, "download_failed"},
		{&engine.Error{Kind: engine.ErrorIntegrity}, "download_failed"},
		{&engine.Error{Kind: engine.ErrorConnectionLost}, "device_connection_interrupted"},
		{&engine.Error{Kind: engine.ErrorDeviceStorageFull}, "device_storage_full"},
		{&engine.Error{Kind: engine.ErrorInternal}, "download_failed"},
		{errors.New("not an engine error"), "download_failed"},
	}
	for _, test := range tests {
		err := newEngineActionError("download_failed", test.err)
		if action, ok := errors.AsType[*domain.ActionError](err); !ok || action.Code != test.want {
			t.Errorf("newEngineActionError(%v) = %v, want code %q", test.err, err, test.want)
		}
	}
}
