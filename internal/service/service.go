package service

import (
	"context"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/library"
	"github.com/wizier/airvault/internal/objectstore"
	"github.com/wizier/airvault/internal/storage"
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
	Restore     bool     `json:"restore,omitempty"`
	Verify      bool     `json:"verify,omitempty"`
	Auto        bool     `json:"auto,omitempty"` // started by the automatic-backup trigger
	Cancelling  bool     `json:"cancelling,omitempty"`
	Transferred int64    `json:"transferred"`
	Speed       int64    `json:"speed"`
}

type Service struct {
	app     context.Context
	store   *storage.Store
	engine  *engine.Engine
	bus     *events.Bus
	ops     *operationManager
	library *library.Library
	files   *devicefs.Manager
	gallery *galleryIndex
	uploads string // staging directory for uploaded .ipa files

	// wg tracks every supervised operation so shutdown does not close shared
	// storage while device work is still returning.
	wg sync.WaitGroup

	// runMu protects backup/restore business state only. Mux presence and
	// SpringBoard evidence live behind live's separate lock.
	runMu sync.RWMutex
	runs  map[string]*activeRun // by UDID — the in-flight backup/restore, if any
	// The error code of each device's last finished run per kind; success or
	// cancel clears it. Volatile like runs: attempts are not durable.
	lastRunError map[runIdentity]string
	// Written as a run ends, like lastRunError (see recordAutoBackup).
	autoHistory map[string]autoHistory
	live        *deviceRuntimeStore

	// deviceRefreshKick coalesces event-driven metadata refreshes (capacity 1); the
	// single StartWatch worker owns them, so lockdown discovers never overlap.
	deviceRefreshKick chan struct{}
	deviceRefreshMu   sync.Mutex

	// autoBackupKick wakes the automatic-backup trigger when a screen lock
	// state changes (capacity 1).
	autoBackupKick chan struct{}

	// deviceTransitionMu serializes low-level state commits with their domain
	// side effects, so startup, Watch and lock-observer callbacks cannot reorder.
	deviceTransitionMu sync.Mutex

	lockObs *lockObserverMgr
}

// The caller opens and closes the resources; uploads must already exist.
func New(app context.Context, store *storage.Store, eng *engine.Engine,
	objects *objectstore.Store, uploads string, bus *events.Bus) *Service {
	s := &Service{
		app:               app,
		store:             store,
		engine:            eng,
		bus:               bus,
		ops:               newOperationManager(),
		files:             devicefs.New(eng),
		gallery:           newGalleryIndex(),
		uploads:           uploads,
		runs:              map[string]*activeRun{},
		lastRunError:      map[runIdentity]string{},
		autoHistory:       map[string]autoHistory{},
		live:              newDeviceRuntimeStore(),
		deviceRefreshKick: make(chan struct{}, 1),
		autoBackupKick:    make(chan struct{}, 1),
	}
	s.library = library.New(objects, store, s.catalogChanged)
	s.lockObs = newLockObserverMgr(app, eng, s.screenLockSignal)
	return s
}

func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

func (s *Service) Running() []RunProgress {
	s.runMu.RLock()
	defer s.runMu.RUnlock()
	out := make([]RunProgress, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, r.progress)
	}
	return out
}

func (s *Service) lastRunErrors(udid string) map[string]string {
	s.runMu.RLock()
	defer s.runMu.RUnlock()
	var out map[string]string
	for _, kind := range []string{runKindBackup, runKindRestore, runKindVerify} {
		if code, ok := s.lastRunError[runIdentity{udid, kind}]; ok {
			if out == nil {
				out = make(map[string]string, 3)
			}
			out[kind] = code
		}
	}
	return out
}

func (s *Service) clearRunOutcomes(udid string) {
	s.runMu.Lock()
	delete(s.lastRunError, runIdentity{udid, runKindBackup})
	delete(s.lastRunError, runIdentity{udid, runKindRestore})
	delete(s.autoHistory, udid)
	s.runMu.Unlock()
}

// Wait reports whether it is safe to close shared storage: false means device
// work is still running, so the DB must not be explicitly closed.
func (s *Service) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		s.library.Wait()
		s.lockObs.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Run IDs are unique, so a delayed browser action cannot cancel a newer run.
func (s *Service) CancelRun(runID string) error {
	if runID == "" {
		return domain.ErrNotFound
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	for _, active := range s.runs {
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
		s.bus.Emit(runProgressed(active.progress))
		return nil
	}
	return domain.ErrNotFound
}
