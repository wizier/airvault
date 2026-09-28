package engine

import (
	"context"
	"sync"

	"github.com/wizier/airvault/internal/ios"
)

// Power is not cancelled by the caller: a sent command cannot be recalled.
func (e *Engine) Power(ctx context.Context, device DeviceID, action PowerAction) error {
	if action > PowerSleep {
		return &Error{Kind: ErrorInvalidArgument, Detail: "bad power action"}
	}
	return do(context.WithoutCancel(ctx), uiCallTimeout, "power", func(ctx context.Context) error {
		conn, err := e.openService(ctx, string(device), ios.DiagnosticsService)
		if err != nil {
			return err
		}
		diagnostics := ios.NewDiagnostics(conn)
		defer diagnostics.Close()
		switch action {
		case PowerRestart:
			return diagnostics.Restart(ctx)
		case PowerShutdown:
			return diagnostics.Shutdown(ctx)
		}
		return diagnostics.Sleep(ctx)
	})
}

// HardwareReport requires only the session: each read is bounded on its own
// and a failed one leaves its part zero.
func (e *Engine) HardwareReport(ctx context.Context, device DeviceID) (HardwareReport, error) {
	return call(ctx, uiCallTimeout, "hardware report", func(ctx context.Context) (HardwareReport, error) {
		session, err := e.openSession(ctx, string(device))
		if err != nil {
			return HardwareReport{}, err
		}
		defer session.Close()
		var report HardwareReport
		var wg sync.WaitGroup
		wg.Go(func() { report.Battery = e.batteryGauge(ctx, string(device)) })

		// The lockdown reads share one connection, and a read that times out
		// leaves it torn: the ones after it are skipped.
		reads := []func(context.Context) error{
			func(ctx context.Context) (err error) {
				report.Lockdown, err = ios.Value[LockdownValues](ctx, session.Lockdown, "", "")
				return err
			},
			func(ctx context.Context) (err error) {
				report.DiskUsage, err = ios.Value[DiskUsage](ctx, session.Lockdown, "com.apple.disk_usage", "")
				return err
			},
			func(ctx context.Context) error {
				associated, err := ios.Value[bool](ctx, session.Lockdown, "com.apple.fmip", "IsAssociated")
				if err == nil {
					report.FindMy = &associated
				}
				return err
			},
		}
		for _, read := range reads {
			readCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			_ = read(readCtx)
			timedOut := readCtx.Err() != nil
			cancel()
			if timedOut {
				break
			}
		}
		wg.Wait()
		return report, nil
	})
}

// batteryGauge is short and best effort: the relay can hang on Wi-Fi.
func (e *Engine) batteryGauge(ctx context.Context, udid string) BatteryGauge {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := e.openService(ctx, udid, ios.DiagnosticsService)
	if err != nil {
		return BatteryGauge{}
	}
	diagnostics := ios.NewDiagnostics(conn)
	defer diagnostics.Close()
	gauge, _ := ios.IORegistry[BatteryGauge](ctx, diagnostics, "AppleSmartBattery")
	return gauge
}

func (e *Engine) Wallpaper(ctx context.Context, device DeviceID, lockScreen bool) ([]byte, error) {
	return call(ctx, uiCallTimeout, "wallpaper", func(ctx context.Context) ([]byte, error) {
		conn, err := e.openService(ctx, string(device), ios.SpringBoardService)
		if err != nil {
			return nil, err
		}
		springboard := ios.NewSpringBoard(conn)
		defer springboard.Close()
		return springboard.WallpaperPNG(ctx, lockScreen)
	})
}
