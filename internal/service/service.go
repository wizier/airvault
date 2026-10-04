package service

import (
	"context"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/library"
	"github.com/wizier/airvault/internal/objectstore"
	"github.com/wizier/airvault/internal/storage"
)

type Service struct {
	app     context.Context
	store   *storage.Store
	engine  *engine.Engine
	bus     *events.Bus
	library *library.Library
	files   *devicefs.Manager
	uploads string

	ops      *operationManager
	runs     *runRegistry
	live     *deviceRuntimeStore
	lockObs  *lockObserverMgr
	gallery  *galleryIndex
	unlocked unlockedBackups
	wg       sync.WaitGroup

	deviceTransitionMu sync.Mutex
	deviceRefreshMu    sync.Mutex
	deviceRefreshKick  chan struct{}
	autoBackupKick     chan struct{}
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
		runs:              newRunRegistry(bus),
		live:              newDeviceRuntimeStore(),
		unlocked:          unlockedBackups{changed: func(source string) { bus.Emit(backupsUnlockedChanged(source)) }},
		deviceRefreshKick: make(chan struct{}, 1),
		autoBackupKick:    make(chan struct{}, 1),
	}
	s.library = library.New(objects, store, s.catalogChanged)
	s.lockObs = newLockObserverMgr(app, eng, s.screenLockSignal)
	return s
}

func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

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

func (s *Service) Running() []RunProgress { return s.runs.list() }

// Run IDs are unique, so a delayed browser action cannot cancel a newer run.
func (s *Service) CancelRun(runID string) error { return s.runs.cancel(runID) }
