package service

import (
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
)

type runPhase uint8

const (
	runPhaseActive runPhase = iota
	runPhaseFinalizing
	runPhaseCommitting
	runPhaseCancelling
)

type activeRun struct {
	run      *runReservation
	phase    runPhase
	progress RunProgress
}

type RunProgress struct {
	RunID       string   `json:"runId"`
	UDID        string   `json:"udid"`
	Percent     int      `json:"progress"`
	Stage       RunStage `json:"stage"`
	Kind        runKind  `json:"kind"`
	Auto        bool     `json:"auto,omitempty"`
	Cancelling  bool     `json:"cancelling,omitempty"`
	Transferred int64    `json:"transferred"`
	Speed       int64    `json:"speed"`
}

// runRegistry holds the in-flight backup, restore and verify runs, one per
// UDID, and the outcomes the finished ones left. None of it is durable.
type runRegistry struct {
	mu          sync.RWMutex
	bus         *events.Bus
	active      map[string]*activeRun
	lastError   map[string]map[runKind]string
	autoHistory map[string]autoHistory
}

func newRunRegistry(bus *events.Bus) *runRegistry {
	return &runRegistry{bus: bus, active: map[string]*activeRun{},
		lastError: map[string]map[runKind]string{}, autoHistory: map[string]autoHistory{}}
}

// From here the run is visible to Running(), progress and cancellation, and it
// must reach a terminal event via completeRun.
func (r *runRegistry) announce(run *runReservation, stage RunStage) error {
	r.mu.Lock()
	if _, busy := r.active[run.udid]; busy {
		r.mu.Unlock()
		return rejectOperation(run.ctx, run.kind, run.udid,
			fmt.Errorf("%w: a run is already active for this device", domain.ErrBusy))
	}
	r.active[run.udid] = &activeRun{run: run, progress: RunProgress{
		RunID: run.id, UDID: run.udid, Stage: stage, Kind: run.kind, Auto: run.auto,
	}}
	r.mu.Unlock()
	logOperationStarted(run.ctx, run.kind, run.udid, run.logAttrs()...)
	return nil
}

// Removing the run and recording its outcome share one critical section, so a
// refetch can never see them disagree.
func (r *runRegistry) hide(run *runReservation, state runState, errorCode string) {
	r.mu.Lock()
	delete(r.active, run.udid)
	failures := r.lastError[run.udid]
	switch {
	case state == runStateFailed && run.auto && errorCode == errorBackupNotConfirmed:
		// An unanswered automatic prompt is expected, not a failure to flag: the
		// automatic-backup status shows the pause it caused.
	case state == runStateFailed && failures == nil:
		r.lastError[run.udid] = map[runKind]string{run.kind: errorCode}
	case state == runStateFailed:
		failures[run.kind] = errorCode
	default:
		delete(failures, run.kind)
	}
	if run.kind == runKindBackup {
		r.recordAutoBackup(run, state, errorCode, time.Now())
	}
	r.mu.Unlock()
}

func (r *runRegistry) list() []RunProgress {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RunProgress, 0, len(r.active))
	for _, active := range r.active {
		out = append(out, active.progress)
	}
	return out
}

func (r *runRegistry) lastErrors(udid string) map[runKind]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return maps.Clone(r.lastError[udid])
}

func (r *runRegistry) forget(udid string) {
	r.mu.Lock()
	delete(r.lastError, udid)
	delete(r.autoHistory, udid)
	r.mu.Unlock()
}

func (r *runRegistry) busy(udid string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, busy := r.active[udid]
	return busy
}

// Speed averages the interval each emit covers, total makes the percentage an
// exact byte ratio, and idleStage names the wait before the first payload byte.
func (r *runRegistry) progressSink(run *runReservation, idleStage, activeStage RunStage, total int64) func(engine.Progress) {
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
		progress := RunProgress{RunID: run.id, UDID: run.udid, Kind: run.kind, Auto: run.auto,
			Percent: percent, Transferred: transferred}
		emit := finalizing || time.Since(lastEmit) >= emitEvery
		if emit && !lastEmit.IsZero() {
			speed = int64(float64(transferred-emitted) / time.Since(lastEmit).Seconds())
		}
		progress.Speed = speed
		r.mu.Lock()
		active := r.active[run.udid]
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
			r.bus.Emit(runProgressed(progress))
		}
		r.mu.Unlock()
	}
}

func (r *runRegistry) transferred(run *runReservation) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if active := r.active[run.udid]; active != nil {
		return active.progress.Transferred
	}
	return 0
}

func (r *runRegistry) setStage(run *runReservation, stage RunStage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := r.active[run.udid]
	if active == nil || active.phase != runPhaseActive {
		return
	}
	active.progress.Stage = stage
	r.bus.Emit(runProgressed(active.progress))
}

// beginCommit is the cancellation boundary: before it a run may be discarded;
// after it the verified result must reach durable storage.
func (r *runRegistry) beginCommit(run *runReservation) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := r.active[run.udid]
	if run.ctx.Err() != nil || active.phase == runPhaseCancelling {
		return false
	}
	active.phase = runPhaseCommitting
	return true
}

func (r *runRegistry) cancel(runID string) error {
	if runID == "" {
		return domain.ErrNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, active := range r.active {
		if active.run.id != runID {
			continue
		}
		if active.phase == runPhaseCommitting {
			return domain.ErrOperationState
		}
		if active.phase != runPhaseCancelling {
			active.phase = runPhaseCancelling
			switch active.run.kind {
			case runKindRestore:
				active.progress.Stage = StageCancellingRestore
			case runKindVerify:
				active.progress.Stage = StageCancellingVerify
			default:
				active.progress.Stage = StageCancellingBackup
			}
			active.progress.Cancelling = true
			active.progress.Speed = 0
		}
		active.run.cancel()
		// Publish while holding the run lock so a terminal event cannot overtake
		// this final progress state and resurrect a completed run in the UI.
		r.bus.Emit(runProgressed(active.progress))
		return nil
	}
	return domain.ErrNotFound
}
