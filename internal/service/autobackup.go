package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/model"
)

// Automatic backups start on the owner's own unlock. iOS asks for the device
// passcode before every host-initiated backup and gives up after about a
// minute, so a backup can only succeed while someone is holding the phone.

const (
	// autoBackupDwell is how long an unlock must last before the trigger fires,
	// so a glance at the phone never raises the passcode prompt.
	autoBackupDwell = 5 * time.Second
	// autoBackupSlack makes the next backup due this much before the full
	// interval, so a daily backup does not drift later day by day.
	autoBackupSlack = 4 * time.Hour
	// A failed automatic attempt pauses the trigger; at most autoBackupLimit
	// of them may fall within autoBackupLimitWindow.
	autoBackupPause       = time.Hour
	autoBackupLimit       = 3
	autoBackupLimitWindow = 24 * time.Hour
)

// The passcode prompt was dismissed or left to time out on the phone.
const errorBackupNotConfirmed = "backup_not_confirmed"

// Why the next automatic backup cannot start yet.
const (
	autoWaitFirstBackup = "first_backup" // the first, full backup is manual
	autoWaitSchedule    = "schedule"
	autoWaitPaused      = "paused"
	autoWaitLimit       = "limit"
)

var autoBackupPresets = []int{1, 3, 7}

type AutoBackupSettings struct {
	Enabled   bool              `json:"enabled"`
	EveryDays int               `json:"everyDays"`
	Window    *AutoBackupWindow `json:"window,omitempty"`
}

// The window may cross midnight. TimeZone is the IANA zone the times are read
// in.
type AutoBackupWindow struct {
	Start    string `json:"start"` // "HH:MM"
	End      string `json:"end"`
	TimeZone string `json:"timeZone"`
}

type AutoBackupView struct {
	AutoBackupSettings
	// Wait names why an enabled automatic backup cannot start yet (see the
	// autoWait* codes); empty when the next unlock inside the window starts it.
	Wait      string     `json:"wait,omitempty"`
	NotBefore *time.Time `json:"notBefore,omitempty"`
}

// A nil clockWindow means any time.
type clockWindow struct {
	start, end int // minutes after local midnight
	loc        *time.Location
}

func (w *clockWindow) contains(t time.Time) bool {
	if w == nil {
		return true
	}
	local := t.In(w.loc)
	minute := local.Hour()*60 + local.Minute()
	if w.start < w.end {
		return w.start <= minute && minute < w.end
	}
	return minute >= w.start || minute < w.end
}

type autoHistory struct {
	failures    [autoBackupLimit]time.Time // oldest first; zero until that many
	pausedUntil time.Time
}

// autoWait ignores the window. It returns the reason and end of the latest
// constraint still in force.
func autoWait(every time.Duration, lastBackup *time.Time, history autoHistory, now time.Time) (string, time.Time) {
	if lastBackup == nil {
		return autoWaitFirstBackup, time.Time{}
	}
	wait, notBefore := "", now
	hold := func(reason string, until time.Time) {
		if until.After(notBefore) {
			wait, notBefore = reason, until
		}
	}
	hold(autoWaitSchedule, lastBackup.Add(every))
	hold(autoWaitPaused, history.pausedUntil)
	if oldest := history.failures[0]; !oldest.IsZero() {
		hold(autoWaitLimit, oldest.Add(autoBackupLimitWindow))
	}
	if wait == "" {
		return "", time.Time{}
	}
	return wait, notBefore
}

func autoBackupEvery(days int) time.Duration {
	return time.Duration(days)*24*time.Hour - autoBackupSlack
}

// A success clears the history. A failed or cancelled automatic attempt counts
// toward the limit and pauses the trigger; an unanswered prompt also pauses it.
// hideRun calls this under runMu, so a run gone from s.runs was recorded.
func (s *Service) recordAutoBackup(run *runReservation, state, errorCode string, now time.Time) {
	switch {
	case state == runStateCompleted:
		delete(s.autoHistory, run.udid)
	case run.auto || errorCode == errorBackupNotConfirmed:
		h := s.autoHistory[run.udid]
		if run.auto {
			copy(h.failures[:], h.failures[1:])
			h.failures[autoBackupLimit-1] = now
		}
		h.pausedUntil = now.Add(autoBackupPause)
		s.autoHistory[run.udid] = h
	}
}

// The history never leaves the lock that guards it.
func (s *Service) autoBackupWait(udid string, every time.Duration, lastBackup *time.Time,
	now time.Time) (string, time.Time) {
	s.runMu.RLock()
	defer s.runMu.RUnlock()
	return autoWait(every, lastBackup, s.autoHistory[udid], now)
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
			if r.presence != "wifi" || r.lockScreen() || fired[udid].Equal(r.unlockedAt) {
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
	if s.runActive(udid) {
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
	window, err := clockWindowOf(settings)
	if err != nil {
		return false, err
	}
	created, err := s.library.LatestCreated(ctx, udid)
	if err != nil {
		return false, err
	}
	now := time.Now()
	wait, _ := s.autoBackupWait(udid, autoBackupEvery(settings.Days), optionalTime(created), now)
	return wait == "" && window.contains(now), nil
}

func (s *Service) runActive(udid string) bool {
	s.runMu.RLock()
	defer s.runMu.RUnlock()
	_, active := s.runs[udid]
	return active
}

func (s *Service) SetAutoBackup(ctx context.Context, udid string, settings AutoBackupSettings) error {
	stored, err := autoBackupRow(settings)
	if err != nil {
		return err
	}
	if err := s.store.Device.SetAutoBackup(ctx, udid, stored); err != nil {
		return err
	}
	s.bus.Emit(deviceUpdated(udid))
	return nil
}

func (s *Service) autoBackupView(d model.Device, lastBackup *time.Time, now time.Time) *AutoBackupView {
	settings := d.AutoBackup
	view := &AutoBackupView{AutoBackupSettings: autoBackupSettingsOf(settings)}
	if !settings.Enabled {
		return view
	}
	wait, notBefore := s.autoBackupWait(d.UDID, autoBackupEvery(settings.Days), lastBackup, now)
	view.Wait = wait
	if !notBefore.IsZero() {
		view.NotBefore = new(notBefore.UTC())
	}
	return view
}

func autoBackupRow(settings AutoBackupSettings) (model.AutoBackup, error) {
	if !slices.Contains(autoBackupPresets, settings.EveryDays) {
		return model.AutoBackup{}, &domain.ValidationError{Code: "invalid_auto_backup_interval",
			Message: fmt.Sprintf("auto backup interval must be one of %v days", autoBackupPresets)}
	}
	row := model.AutoBackup{Enabled: settings.Enabled, Days: settings.EveryDays}
	if window := settings.Window; window != nil {
		start, startErr := parseClock(window.Start)
		end, endErr := parseClock(window.End)
		if startErr != nil || endErr != nil || start == end {
			return model.AutoBackup{}, &domain.ValidationError{Code: "invalid_auto_backup_window",
				Message: "auto backup window needs two different HH:MM times"}
		}
		if _, err := loadWindowZone(window.TimeZone); err != nil {
			return model.AutoBackup{}, &domain.ValidationError{Code: "invalid_time_zone", Message: err.Error()}
		}
		row.WindowStart, row.WindowEnd, row.TimeZone = &start, &end, window.TimeZone
	}
	return row, nil
}

func autoBackupSettingsOf(stored model.AutoBackup) AutoBackupSettings {
	settings := AutoBackupSettings{Enabled: stored.Enabled, EveryDays: stored.Days}
	if stored.WindowStart != nil && stored.WindowEnd != nil {
		settings.Window = &AutoBackupWindow{Start: formatClock(*stored.WindowStart),
			End: formatClock(*stored.WindowEnd), TimeZone: stored.TimeZone}
	}
	return settings
}

func clockWindowOf(stored model.AutoBackup) (*clockWindow, error) {
	if stored.WindowStart == nil || stored.WindowEnd == nil {
		return nil, nil
	}
	loc, err := loadWindowZone(stored.TimeZone)
	if err != nil {
		return nil, err
	}
	return &clockWindow{start: int(*stored.WindowStart), end: int(*stored.WindowEnd), loc: loc}, nil
}

// loadWindowZone refuses the empty name time.LoadLocation reads as UTC.
func loadWindowZone(name string) (*time.Location, error) {
	if name == "" {
		return nil, errors.New("auto backup window needs a time zone")
	}
	return time.LoadLocation(name)
}

func parseClock(value string) (int64, error) {
	clock, err := time.Parse("15:04", value)
	return int64(clock.Hour()*60 + clock.Minute()), err
}

func formatClock(minutes int64) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}
