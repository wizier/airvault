// Package service orchestrates device discovery and backups on top of the
// engine and storage layers, and holds the live in-flight backup progress.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/config"
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
	live         *deviceRuntimeStore

	// deviceRefreshKick coalesces event-driven metadata refreshes (capacity 1); the
	// single StartWatch worker owns them, so lockdown discovers never overlap.
	deviceRefreshKick chan struct{}
	deviceRefreshMu   sync.Mutex

	// deviceTransitionMu serializes low-level state commits with their domain
	// side effects, so startup, Watch and lock-observer callbacks cannot reorder.
	deviceTransitionMu sync.Mutex

	// lockObs is set in New; its workers live on the app context.
	lockObs *lockObserverMgr
}

// New builds a Service.
func New(app context.Context, store *storage.Store, eng *engine.Engine, cfg *config.Config, bus *events.Bus) (*Service, error) {
	objects, err := objectstore.New(cfg.BackupDir)
	if err != nil {
		return nil, err
	}
	// Upload staging lives on a mounted volume (multi-GiB .ipa files must not
	// land in the container's writable layer); leftovers are cleared on start.
	uploads := filepath.Join(cfg.ConfigDir, "uploads")
	if err := os.RemoveAll(uploads); err != nil {
		return nil, fmt.Errorf("clear upload staging: %w", err)
	}
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		return nil, fmt.Errorf("create upload staging: %w", err)
	}
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
		live:              newDeviceRuntimeStore(),
		deviceRefreshKick: make(chan struct{}, 1),
	}
	s.lockObs = newLockObserverMgr(app, eng, s.screenLockSignal)
	return s, nil
}

// MuxerReady reports whether the device muxer is reachable.
func (s *Service) MuxerReady() bool {
	state, err := s.engine.ProbeMux(s.app)
	return err == nil && state == engine.MuxAvailable
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

func (s *Service) clearLastRunErrors(udid string) {
	s.runMu.Lock()
	delete(s.lastRunError, runIdentity{udid, runKindBackup})
	delete(s.lastRunError, runIdentity{udid, runKindRestore})
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

// Close releases process-wide resources after all service workers have joined.
func (s *Service) Close() error {
	engineErr := s.engine.Close()
	return errors.Join(engineErr, s.objects.Close())
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
