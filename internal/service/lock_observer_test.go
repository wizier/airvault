package service

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/wizier/airvault/internal/engine"
)

// fakeLockEngine opens lock streams that stay silent until their observer is
// cancelled, recording each open and close.
type fakeLockEngine struct {
	opened, closed chan struct{}
}

func (f fakeLockEngine) OpenLockObserver(ctx context.Context, _ engine.DeviceID) (engine.Stream[engine.ScreenLockSignal], error) {
	f.opened <- struct{}{}
	return fakeLockStream{ctx: ctx, closed: f.closed}, nil
}

type fakeLockStream struct {
	ctx    context.Context
	closed chan struct{}
}

func (s fakeLockStream) Next() (engine.ScreenLockSignal, error) {
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}

func (s fakeLockStream) Close() error {
	s.closed <- struct{}{}
	return nil
}

// The lock observer runs while a device is online and stops when it goes offline.
func TestLockObserverLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := fakeLockEngine{opened: make(chan struct{}, 2), closed: make(chan struct{}, 2)}
		m := newLockObserverMgr(context.Background(), fake, func(string, engine.ScreenLockSignal) {})
		m.setOnline("phone")
		m.setOnline("phone") // idempotent: an already-running observer is not doubled
		synctest.Wait()
		if got := len(fake.opened); got != 1 {
			t.Fatalf("online device opened %d lock observers, want 1", got)
		}
		m.setOffline("phone")
		synctest.Wait()
		if got := len(fake.closed); got != 1 {
			t.Fatalf("offline device closed %d lock observers, want 1", got)
		}
	})
}
