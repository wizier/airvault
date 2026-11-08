package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/storage"
)

// Automatic integrity checks need no phone, so the server starts them itself:
// once a source's last check is a schedule old, inside the device's window.

const autoVerifyTick = time.Minute // windows have minute resolution

var autoVerifyPresets = []int{7, 14, 30}

// A completed check clears the pause, whoever started it. A manual one that
// failed leaves the schedule as it is; an automatic one that failed or was
// cancelled tries again in the next day's window.
func (r *runRegistry) recordAutoVerify(run *runReservation, state runState, now time.Time) {
	key := autoKey{run.udid, runKindVerify}
	switch {
	case state == runStateCompleted:
		delete(r.autoHistory, key)
	case run.auto:
		r.autoHistory[key] = autoHistory{pausedUntil: now.Add(autoEvery(1))}
	}
}

// Runs on a goroutine s.wg tracks, so the runs it starts never race Wait.
func (s *Service) runAutoVerifyTrigger(ctx context.Context) {
	ticker := time.NewTicker(autoVerifyTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := s.startDueVerify(ctx, now); err != nil {
				slog.WarnContext(ctx, "auto-verify: looking for a due check", "error", err)
			}
		}
	}
}

func (s *Service) startDueVerify(ctx context.Context, now time.Time) error {
	devices, err := s.store.Device.List(ctx)
	if err != nil {
		return err
	}
	summary, err := s.library.Summary(ctx)
	if err != nil {
		return err
	}
	for _, device := range devices {
		if !s.autoVerifyDue(device, summary[device.UDID], now) {
			continue
		}
		if _, err := s.startVerify(device.UDID, true); err != nil {
			slog.DebugContext(ctx, "auto-verify: not started", "udid", device.UDID, "error", err)
		}
	}
	return nil
}

func (s *Service) autoVerifyDue(device model.Device, backups storage.SourceSummary, now time.Time) bool {
	settings := device.AutoVerify
	// Another run holds the leases: reserving would only log a rejection.
	if !settings.Enabled || s.runs.busy(device.UDID) {
		return false
	}
	wait, _ := s.runs.scheduleWait(runKindVerify, device.UDID, settings, backups, now)
	return wait == "" && clockWindowOf(settings).contains(now)
}
