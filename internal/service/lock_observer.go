package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/engine"
)

// The notification_proxy socket dies with the muxer attachment; the next online
// transition restarts the observer.
type lockObserverMgr struct {
	base   context.Context
	engine lockObservation
	onLock func(udid string, signal engine.ScreenLockSignal)

	mu      sync.Mutex
	workers map[string]context.CancelFunc
	wg      sync.WaitGroup
}

type lockObservation interface {
	OpenLockObserver(context.Context, engine.DeviceID) (engine.Stream[engine.ScreenLockSignal], error)
}

func newLockObserverMgr(base context.Context, eng lockObservation,
	onLock func(string, engine.ScreenLockSignal)) *lockObserverMgr {
	return &lockObserverMgr{
		base:    base,
		engine:  eng,
		onLock:  onLock,
		workers: map[string]context.CancelFunc{},
	}
}

func (m *lockObserverMgr) setOnline(udid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.base.Err() != nil || m.workers[udid] != nil {
		return
	}
	ctx, cancel := context.WithCancel(m.base)
	m.workers[udid] = cancel
	m.wg.Go(func() { m.run(ctx, udid) })
}

func (m *lockObserverMgr) setOffline(udid string) {
	m.mu.Lock()
	cancel := m.workers[udid]
	delete(m.workers, udid)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *lockObserverMgr) run(ctx context.Context, udid string) {
	const initialRetry = 3 * time.Second
	const maxRetry = time.Minute
	retry := initialRetry
	for ctx.Err() == nil {
		stream, err := m.engine.OpenLockObserver(ctx, engine.DeviceID(udid))
		if err == nil {
			retry = initialRetry
			for ctx.Err() == nil {
				signal, nextErr := stream.Next()
				if nextErr != nil {
					err = nextErr
					break
				}
				if ctx.Err() != nil {
					break
				}
				m.onLock(udid, signal)
			}
			_ = stream.Close()
		}
		if err != nil && ctx.Err() == nil {
			slog.Debug("lock observer ended", "udid", udid, "error", err)
		}
		if !sleepContext(ctx, retry) {
			return
		}
		retry = min(retry*2, maxRetry)
	}
}
