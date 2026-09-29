package engine

import (
	"context"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/backup2"
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

// EraseDevice erases all content and settings over mobilebackup2, as Finder
// does. Once sent it cannot be recalled, so the caller cannot cancel the wait;
// the phone may ask for its passcode first, and hanging up means it began.
func (e *Engine) EraseDevice(ctx context.Context, device DeviceID) error {
	udid := string(device)
	if err := validateUDID(udid); err != nil {
		return err
	}
	if err := e.checkFindMy(ctx, udid); err != nil {
		return err
	}
	conn, err := e.openBackup2(ctx, udid)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Erase(ctx); err != nil {
		return failure(ctx, "erase", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), passwordPromptTimeout)
	defer cancel()
	outcome, err := conn.Outcome(ctx)
	switch {
	case err != nil && classify(ctx, err) == ErrorConnectionLost, err == nil && outcome == nil:
		return nil
	case err != nil:
		return failure(ctx, "erase", err)
	}
	return verdictFailure(backup2.Verdict(outcome), false)
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
