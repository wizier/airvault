package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/engine"
)

// lockObserverMgr runs one SpringBoard lock observer per online Wi-Fi device,
// started and stopped on presence transitions. The notification_proxy socket
// dies with the muxer attachment; the next online transition restarts it.
type lockObserverMgr struct {
	base   context.Context // app lifetime — observer goroutines die with it
	engine lockObservation
	onLock func(udid string, signal engine.ScreenLockSignal)

	mu      sync.Mutex
	workers map[string]context.CancelFunc
	wg      sync.WaitGroup
}

type lockObservation interface {
	OpenLockObserver(context.Context, engine.DeviceID) (engine.LockStream, error)
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

// setOnline starts a lock observer for udid unless one is already running.
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

// setOffline stops udid's lock observer if one is running.
func (m *lockObserverMgr) setOffline(udid string) {
	m.mu.Lock()
	cancel := m.workers[udid]
	delete(m.workers, udid)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// run keeps a lock observer up while the device is online, re-establishing with
// backoff (reset on a successful open) until setOffline or shutdown cancels ctx.
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
