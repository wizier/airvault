package engine

import (
	"context"
	"io"
	"sync"

	"github.com/wizier/airvault/internal/ios"
)

// Stream is a device stream. The Open context and Close both end it; it fails
// once, then reports io.ErrClosedPipe.
type Stream[T any] interface {
	Next() (T, error)
	Close() error
}

type pull[T any] struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	next   func(context.Context) (T, error)
	close  func()

	mu     sync.Mutex // serializes next with Close
	failed bool
}

func newPull[T any](ctx context.Context, next func(context.Context) (T, error), close func()) *pull[T] {
	ctx, cancel := context.WithCancelCause(ctx)
	return &pull[T]{ctx: ctx, cancel: cancel, next: next, close: close}
}

func (p *pull[T]) Next() (T, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var zero T
	if p.ctx.Err() != nil {
		return zero, context.Cause(p.ctx)
	}
	if p.failed {
		return zero, io.ErrClosedPipe
	}
	value, err := p.next(p.ctx)
	if err != nil {
		p.failed = true
		if p.ctx.Err() != nil {
			return zero, context.Cause(p.ctx)
		}
		return zero, err
	}
	return value, nil
}

func (p *pull[T]) Close() error {
	p.cancel(io.ErrClosedPipe)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.close != nil {
		p.close()
		p.close = nil
	}
	return nil
}

// ConsoleLine is one structured record from the device's os_trace stream.
type ConsoleLine struct {
	Timestamp string `json:"ts"`
	Level     string `json:"level"` // notice | info | debug | error | fault
	Pid       uint32 `json:"pid"`
	Image     string `json:"image"`
	Message   string `json:"message"`
	Subsystem string `json:"subsystem,omitempty"`
	Category  string `json:"category,omitempty"`
}

func (e *Engine) OpenConsole(ctx context.Context, device DeviceID) (Stream[ConsoleLine], error) {
	trace, err := call(ctx, uiCallTimeout, "console", func(ctx context.Context) (*ios.OSTrace, error) {
		conn, err := e.openService(ctx, string(device), ios.OSTraceService)
		if err != nil {
			return nil, err
		}
		trace, err := ios.StartOSTrace(ctx, conn)
		if err != nil {
			_ = conn.Close()
		}
		return trace, err
	})
	if err != nil {
		return nil, err
	}
	next := func(ctx context.Context) (ConsoleLine, error) {
		record, err := trace.Next(ctx)
		if err != nil {
			return ConsoleLine{}, failure(ctx, "console", err)
		}
		return ConsoleLine{
			Timestamp: record.Time.UTC().Format("15:04:05.000"),
			Level:     record.Level,
			Pid:       record.PID,
			Image:     record.Image,
			Message:   record.Message,
			Subsystem: record.Subsystem,
			Category:  record.Category,
		}, nil
	}
	return newPull(ctx, next, func() { _ = trace.Close() }), nil
}

type ScreenLockSignal uint8

const (
	ScreenLockChanged ScreenLockSignal = iota + 1
	ScreenLockComplete
)

const (
	lockStateChanged = "com.apple.springboard.lockstate"
	lockComplete     = "com.apple.springboard.lockcomplete"
)

func (e *Engine) OpenLockObserver(ctx context.Context, device DeviceID) (Stream[ScreenLockSignal], error) {
	proxy, err := call(ctx, connectTimeout, "lock observer", func(ctx context.Context) (*ios.NotificationProxy, error) {
		conn, err := e.openService(ctx, string(device), ios.NotificationProxyService)
		if err != nil {
			return nil, err
		}
		proxy := ios.NewNotificationProxy(conn)
		if err := proxy.Observe(ctx, lockStateChanged, lockComplete); err != nil {
			_ = proxy.Close()
			return nil, err
		}
		return proxy, nil
	})
	if err != nil {
		return nil, err
	}
	next := func(ctx context.Context) (ScreenLockSignal, error) {
		for {
			name, err := proxy.Next(ctx)
			switch {
			case err != nil:
				return 0, failure(ctx, "lock observer", err)
			case name == lockStateChanged:
				return ScreenLockChanged, nil
			case name == lockComplete:
				return ScreenLockComplete, nil
			}
		}
	}
	return newPull(ctx, next, func() { _ = proxy.Close() }), nil
}
