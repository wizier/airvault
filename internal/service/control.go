package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
)

// No event is emitted: the watcher reports the resulting detach/reattach as
// ordinary presence changes.
func (s *Service) Power(ctx context.Context, udid, action string) error {
	var engineAction engine.PowerAction
	switch action {
	case "restart":
		engineAction = engine.PowerRestart
	case "shutdown":
		engineAction = engine.PowerShutdown
	case "sleep":
		engineAction = engine.PowerSleep
	default:
		return &domain.ValidationError{Code: "invalid_power_action", Message: "action must be restart, shutdown or sleep"}
	}
	if err := s.reachableDevice(ctx, udid); err != nil {
		return err
	}
	err := s.runCommand(ctx, runKindPower, udid, func(ctx context.Context) error {
		if err := s.engine.Power(ctx, engine.DeviceID(udid), engineAction); err != nil {
			slog.DebugContext(ctx, "power: request failed", "udid", udid, "action", action, "error", err)
			return fmt.Errorf("power %s: %w", action, domain.ErrDeviceOffline)
		}
		slog.DebugContext(ctx, "power: accepted", "udid", udid, "action", action)
		return nil
	}, deviceWriteResource(udid))
	return err
}

// EraseDevice erases the phone as Finder does. The erased phone no longer knows
// this host, so it leaves AirVault; its backups stay, restorable onto any phone.
func (s *Service) EraseDevice(ctx context.Context, udid string) error {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return err
	}
	return s.runCommand(ctx, runKindErase, udid, func(ctx context.Context) error {
		if err := s.engine.EraseDevice(ctx, engine.DeviceID(udid)); err != nil {
			return newTransferActionError("erase_failed", err)
		}
		slog.InfoContext(ctx, "erase: the phone is erasing", "udid", udid)
		finalCtx := context.WithoutCancel(ctx)
		if err := s.engine.ForgetPairing(engine.DeviceID(udid)); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrPairingCleanup, err)
		}
		if err := s.forgetDevice(finalCtx, udid); err != nil {
			return err
		}
		s.bus.Emit(pairingChanged(udid, false))
		return nil
	}, deviceWriteResource(udid))
}

// Engine records whose JSON shape is already the API's are served as they are.
type (
	Battery     = engine.Battery
	ConsoleLine = engine.ConsoleLine
)

// Passive telemetry: the read never wakes the phone and never drives presence.
func (s *Service) LiveBattery(ctx context.Context, udid string) (Battery, error) {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return Battery{}, err
	}
	battery, err := s.engine.Battery(ctx, engine.DeviceID(udid))
	if err != nil {
		return Battery{}, newEngineActionError("battery_query_failed", err)
	}
	return battery, nil
}

func (s *Service) Wallpaper(ctx context.Context, udid string, lockScreen bool) ([]byte, error) {
	// Loaded automatically by device cards; never wakes the phone.
	if err := s.reachableDevice(ctx, udid); err != nil {
		return nil, err
	}
	png, err := s.engine.Wallpaper(ctx, engine.DeviceID(udid), lockScreen)
	if err != nil {
		slog.DebugContext(ctx, "wallpaper: engine", "udid", udid, "error", err)
		return nil, domain.ErrNotFound
	}
	return png, nil
}

func (s *Service) Console(ctx context.Context, udid string, onLine func(ConsoleLine)) error {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return err
	}
	stream, err := s.engine.OpenConsole(ctx, engine.DeviceID(udid))
	if err != nil {
		return newEngineActionError("console_stream_failed", err)
	}
	defer stream.Close()
	for ctx.Err() == nil {
		line, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.ErrClosedPipe) || ctx.Err() != nil {
				return nil
			}
			slog.WarnContext(ctx, "console: engine", "udid", udid, "error", err)
			return newEngineActionError("console_stream_failed", err)
		}
		onLine(line)
	}
	return nil
}
