package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/library"
	"github.com/wizier/airvault/internal/objectstore"
	"github.com/wizier/airvault/internal/storage"
)

// newTestService is a Service with only what running a run touches.
func newTestService() *Service {
	bus := events.New()
	return &Service{app: context.Background(), bus: bus, runs: newRunRegistry(bus)}
}

// registerRun installs backup run "run-1" on "udid-1" in phase Active, as
// reserveRun would.
func registerRun(s *Service) *runReservation {
	ctx, cancel := context.WithCancel(context.Background())
	run := &runReservation{id: "run-1", udid: "udid-1", kind: runKindBackup,
		ctx: ctx, cancel: cancel}
	s.runs.active[run.udid] = &activeRun{
		run:      run,
		progress: RunProgress{RunID: run.id, UDID: run.udid, Stage: StageBackingUp},
	}
	return run
}

// newStoredService is a Service over a real catalog and object store; it
// returns the store's root.
func newStoredService(t *testing.T) (*Service, string) {
	t.Helper()
	store, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	objects, err := objectstore.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = objects.Close()
		_ = store.Close()
	})
	svc := &Service{store: store, bus: events.New(), live: newDeviceRuntimeStore(), ops: newOperationManager()}
	svc.library = library.New(objects, store, svc.catalogChanged)
	return svc, root
}
