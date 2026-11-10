package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
)

// Automatic backups start on the owner's own unlock. iOS asks for the device
// passcode before every host-initiated backup and gives up after about a
// minute, so a backup can only succeed while someone is holding the phone.

const (
	// autoBackupDwell is how long an unlock must last before the trigger fires,
	// so a glance at the phone never raises the passcode prompt.
	autoBackupDwell = 5 * time.Second
	// A failed automatic attempt pauses the trigger; at most autoBackupLimit
	// of them may fall within autoBackupLimitWindow.
	autoBackupPause       = time.Hour
	autoBackupLimit       = 3
	autoBackupLimitWindow = 24 * time.Hour
)

var autoBackupPresets = []int{1, 3, 7}

// A success clears the history. A failed or cancelled automatic attempt counts
// toward the limit and pauses the trigger; an unanswered prompt also pauses it.
// hide calls this under the lock, so a run gone from the registry was recorded.
func (r *runRegistry) recordAutoBackup(run *runReservation, state runState, errorCode domain.Code, now time.Time) {
	key := autoKey{run.udid, runKindBackup}
	switch {
	case state == runStateCompleted:
		delete(r.autoHistory, key)
	case run.auto || errorCode == domain.CodeBackupNotConfirmed:
		h := r.autoHistory[key]
		if run.auto {
			copy(h.failures[:], h.failures[1:])
			h.failures[autoBackupLimit-1] = now
		}
		h.pausedUntil = now.Add(autoBackupPause)
		r.autoHistory[key] = h
	}
}

func (s *Service) wakeAutoBackup() {
	select {
	case s.autoBackupKick <- struct{}{}:
	default:
	}
}

// Runs on a goroutine s.wg tracks, so the runs it starts never race Wait.
func (s *Service) runAutoBackupTrigger(ctx context.Context, fire func(ctx context.Context, udid string)) {
	fired := map[string]time.Time{} // udid → the unlock already acted on
	for {
		now := time.Now()
		var next time.Time
		for udid, r := range s.live.snapshot() {
			if r.presence != engine.ConnectionWiFi || r.lockScreen() || fired[udid].Equal(r.unlockedAt) {
				continue
			}
			if deadline := r.unlockedAt.Add(autoBackupDwell); deadline.After(now) {
				if next.IsZero() || deadline.Before(next) {
					next = deadline
				}
				continue
			}
			fired[udid] = r.unlockedAt
			fire(ctx, udid)
		}
		var elapsed <-chan time.Time
		if !next.IsZero() {
			elapsed = time.After(time.Until(next))
		}
		select {
		case <-ctx.Done():
			return
		case <-s.autoBackupKick:
		case <-elapsed:
		}
	}
}

func (s *Service) fireAutoBackup(ctx context.Context, udid string) {
	// Our own run holds the leases: reserving would only log a rejection. With
	// no run active, the last one's outcome is already in the history.
	if s.runs.busy(udid) {
		return
	}
	due, err := s.autoBackupDue(ctx, udid)
	if err == nil && due {
		_, err = s.startBackup(ctx, udid, true)
	}
	if err != nil {
		slog.DebugContext(ctx, "auto-backup: not started", "udid", udid, "error", err)
	}
}

func (s *Service) autoBackupDue(ctx context.Context, udid string) (bool, error) {
	device, err := s.pairedDevice(ctx, udid)
	if err != nil {
		return false, err
	}
	settings := device.AutoBackup
	if !settings.Enabled {
		return false, nil
	}
	summary, err := s.library.Summary(ctx)
	if err != nil {
		return false, err
	}
	now := time.Now()
	wait, _ := s.runs.scheduleWait(runKindBackup, udid, settings, summary[udid], now)
	return wait == "" && clockWindowOf(settings).contains(now), nil
}
