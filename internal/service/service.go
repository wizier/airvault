// Package service orchestrates device discovery and backups on top of the
// engine and storage layers, and holds the live in-flight backup progress.
package service

import (
	"context"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
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

// activeRun is the live projection of one reserved backup or restore.
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
	Auto        bool     `json:"auto,omitempty"` // started by the automatic-backup trigger
	Cancelling  bool     `json:"cancelling,omitempty"`
	Transferred int64    `json:"transferred"`
	Speed       int64    `json:"speed"`
}

// Service is the backup/device orchestrator.
type Service struct {
	app     context.Context
	store   *storage.Store
	engine  *engine.Engine
	bus     *events.Bus
	ops     *operationManager
	objects *objectstore.Store
	files   *devicefs.Manager
	gallery *galleryIndex
	uploads string // staging directory for uploaded .ipa files

	// wg tracks every supervised operation so shutdown does not close shared
	// storage while native work is still returning.
	wg sync.WaitGroup

	// runMu protects backup/restore business state only. Mux presence and
	// SpringBoard evidence live behind live's separate lock.
	runMu sync.RWMutex
	runs  map[string]*activeRun // by UDID — the in-flight backup/restore, if any
	// lastRunError holds the error code of a device's most recent finished run,
	// per kind; success or cancel clears that kind. Volatile like runs:
	// attempts are not durable.
	lastRunError map[runIdentity]string
	// autoHistory holds each device's recent automatic-backup setbacks; like
	// lastRunError it is written as a run ends (see recordAutoBackup).
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

	// lockObs is set in New; its workers live on the app context.
	lockObs *lockObserverMgr
}

// New builds a Service over resources the caller opened and closes. uploads is
// an existing directory for staging uploaded .ipa files.
func New(app context.Context, store *storage.Store, eng *engine.Engine,
	objects *objectstore.Store, uploads string, bus *events.Bus) *Service {
	s := &Service{
		app:               app,
		store:             store,
		engine:            eng,
		bus:               bus,
		ops:               newOperationManager(),
		objects:           objects,
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
	s.lockObs = newLockObserverMgr(app, eng, s.screenLockSignal)
	return s
}

// MuxerReady reports whether the device muxer is reachable.
func (s *Service) MuxerReady(ctx context.Context) bool {
	up, err := s.engine.ProbeMux(ctx)
	return err == nil && up
}

// Ping reports storage reachability (for /healthz).
func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

// Running returns snapshots of all in-flight backups.
func (s *Service) Running() []RunProgress {
	s.runMu.RLock()
	defer s.runMu.RUnlock()
	out := make([]RunProgress, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, r.progress)
	}
	return out
}

// lastRunErrors returns the device's per-kind last failure codes; nil when clean.
func (s *Service) lastRunErrors(udid string) map[string]string {
	s.runMu.RLock()
	defer s.runMu.RUnlock()
	var out map[string]string
	for _, kind := range []string{runKindBackup, runKindRestore} {
		if code, ok := s.lastRunError[runIdentity{udid, kind}]; ok {
			if out == nil {
				out = make(map[string]string, 2)
			}
			out[kind] = code
		}
	}
	return out
}

// clearRunOutcomes forgets a device's recorded run outcomes.
func (s *Service) clearRunOutcomes(udid string) {
	s.runMu.Lock()
	delete(s.lastRunError, runIdentity{udid, runKindBackup})
	delete(s.lastRunError, runIdentity{udid, runKindRestore})
	delete(s.autoHistory, udid)
	s.runMu.Unlock()
}

// Wait blocks until supervised work finalizes (bounded by timeout) and reports
// whether it is safe to close shared storage — a false result means native work
// is still running, so the DB must not be explicitly closed.
func (s *Service) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
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

// CancelRun aborts the exact in-flight backup or restore. Runtime IDs are
// unique, so a delayed browser action cannot cancel a newer run.
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
			if active.run.kind == runKindRestore {
				active.progress.Stage = StageCancellingRestore
			} else {
				active.progress.Stage = StageCancellingBackup
			}
			active.progress.Cancelling = true
			active.progress.Speed = 0
		}
		active.run.cancel()
		// Publish while holding the run lock so a terminal event cannot overtake
		// this final progress state and resurrect a completed run in the UI.
		s.bus.Emit(events.BackupProgress, active.progress)
		return nil
	}
	return domain.ErrNotFound
}
