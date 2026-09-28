package engine

import (
	"context"

	"github.com/wizier/airvault/internal/ios"
)

type PowerAction uint8

const (
	PowerRestart PowerAction = iota
	PowerShutdown
	PowerSleep
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
