package engine

import (
	"context"

	"github.com/wizier/airvault/internal/ios"
)

type ConsoleStream struct {
	*pull[ConsoleLine]
}

func (e *Engine) OpenConsole(ctx context.Context, device DeviceID) (*ConsoleStream, error) {
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
	return &ConsoleStream{newPull(ctx, next, func() { _ = trace.Close() })}, nil
}

const (
	lockStateChanged = "com.apple.springboard.lockstate"
	lockComplete     = "com.apple.springboard.lockcomplete"
)

func (e *Engine) OpenLockObserver(ctx context.Context, device DeviceID) (LockStream, error) {
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
