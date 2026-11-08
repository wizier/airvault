package service

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/storage"
)

// A device's automatic tasks are saved as one. Backups and integrity checks share
// the shape of their schedule; what starts them and how they fail stays their own.

// autoSlack makes a scheduled task due this much before its full interval, so a
// daily backup does not drift later day by day.
const autoSlack = 4 * time.Hour

// Why a scheduled task cannot start yet.
const (
	autoWaitFirstBackup = "first_backup" // the first, full backup is manual
	autoWaitSchedule    = "schedule"
	autoWaitPaused      = "paused"
	autoWaitLimit       = "limit"
)

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

type AutomaticTasks struct {
	AutoBackup ScheduleSettings `json:"autoBackup"`
	Cleanup    CleanupSettings  `json:"cleanup"`
	AutoVerify ScheduleSettings `json:"autoVerify"`
}

type ScheduleSettings struct {
	Enabled   bool        `json:"enabled"`
	EveryDays int         `json:"everyDays"`
	Window    *AutoWindow `json:"window,omitempty"`
}

// The window may cross midnight; its times are in the server's zone (TZ).
type AutoWindow struct {
	Start string `json:"start"` // "HH:MM"
	End   string `json:"end"`
}

type ScheduleView struct {
	ScheduleSettings
	// TimeZone names the server's zone the window is read in.
	TimeZone string `json:"timeZone"`
	// Wait names why an enabled task cannot start yet (see the autoWait* codes);
	// empty when only the window and the task's own trigger hold it.
	Wait      string     `json:"wait,omitempty"`
	NotBefore *time.Time `json:"notBefore,omitempty"`
}

type autoHistory struct {
	failures    [autoBackupLimit]time.Time // oldest first; zero until that many
	pausedUntil time.Time
}

// Each scheduled task of a device keeps its own history.
type autoKey struct {
	udid string
	kind runKind
}

func (s *Service) SetAutomaticTasks(ctx context.Context, udid string, tasks AutomaticTasks) error {
	row, err := automaticTasksRow(tasks)
	if err != nil {
		return err
	}
	if err := s.store.Device.SetAutomaticTasks(ctx, udid, row); err != nil {
		return err
	}
	s.bus.Emit(deviceUpdated(udid))
	return nil
}

func automaticTasksRow(tasks AutomaticTasks) (model.AutomaticTasks, error) {
	autoBackup, err := scheduleRow(tasks.AutoBackup, autoBackupPresets, "invalid_auto_backup_interval")
	if err != nil {
		return model.AutomaticTasks{}, err
	}
	cleanup, err := cleanupRow(tasks.Cleanup)
	if err != nil {
		return model.AutomaticTasks{}, err
	}
	autoVerify, err := scheduleRow(tasks.AutoVerify, autoVerifyPresets, "invalid_auto_verify_interval")
	if err != nil {
		return model.AutomaticTasks{}, err
	}
	return model.AutomaticTasks{Cleanup: cleanup, AutoBackup: autoBackup, AutoVerify: autoVerify}, nil
}

func scheduleRow(settings ScheduleSettings, presets []int, intervalCode string) (model.Schedule, error) {
	if !slices.Contains(presets, settings.EveryDays) {
		return model.Schedule{}, &domain.ValidationError{Code: intervalCode,
			Message: fmt.Sprintf("the interval must be one of %v days", presets)}
	}
	row := model.Schedule{Enabled: settings.Enabled, Days: settings.EveryDays}
	if settings.Window == nil {
		return row, nil
	}
	start, startErr := parseClock(settings.Window.Start)
	end, endErr := parseClock(settings.Window.End)
	if startErr != nil || endErr != nil || start == end {
		return model.Schedule{}, &domain.ValidationError{Code: "invalid_auto_window",
			Message: "the window needs two different HH:MM times"}
	}
	row.WindowStart, row.WindowEnd = &start, &end
	return row, nil
}

func scheduleSettingsOf(stored model.Schedule) ScheduleSettings {
	settings := ScheduleSettings{Enabled: stored.Enabled, EveryDays: stored.Days}
	if stored.WindowStart != nil && stored.WindowEnd != nil {
		settings.Window = &AutoWindow{Start: formatClock(*stored.WindowStart), End: formatClock(*stored.WindowEnd)}
	}
	return settings
}

// The wait only matters while the task is on.
func (s *Service) scheduleView(kind runKind, udid string, stored model.Schedule, backups storage.SourceSummary,
	now time.Time) *ScheduleView {
	view := &ScheduleView{ScheduleSettings: scheduleSettingsOf(stored), TimeZone: serverZone()}
	if !stored.Enabled {
		return view
	}
	wait, notBefore := s.runs.scheduleWait(kind, udid, stored, backups, now)
	view.Wait = wait
	if !notBefore.IsZero() {
		view.NotBefore = new(notBefore.UTC())
	}
	return view
}

func clockWindowOf(stored model.Schedule) *clockWindow {
	if stored.WindowStart == nil || stored.WindowEnd == nil {
		return nil
	}
	return &clockWindow{start: int(*stored.WindowStart), end: int(*stored.WindowEnd), loc: time.Local}
}

// A backup's schedule counts from the latest restorable one, so a source whose
// backups are all damaged is due; a check's from the last finished one, so a source
// never checked is due. The history never leaves the lock that guards it.
func (r *runRegistry) scheduleWait(kind runKind, udid string, schedule model.Schedule, backups storage.SourceSummary,
	now time.Time) (string, time.Time) {
	latest := backups.LatestRestorable
	if kind == runKindVerify {
		latest = backups.LatestVerified
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return autoWait(autoEvery(schedule.Days), backups.RestorePoints, latest, r.autoHistory[autoKey{udid, kind}], now)
}

// autoWait ignores the window. It returns the reason and end of the latest
// constraint still in force, for a source with points restore points whose task
// last ran at latest.
func autoWait(every time.Duration, points int, latest *int64, history autoHistory, now time.Time) (string, time.Time) {
	if points == 0 {
		return autoWaitFirstBackup, time.Time{}
	}
	wait, notBefore := "", now
	hold := func(reason string, until time.Time) {
		if until.After(notBefore) {
			wait, notBefore = reason, until
		}
	}
	if latest != nil {
		hold(autoWaitSchedule, time.Unix(*latest, 0).Add(every))
	}
	hold(autoWaitPaused, history.pausedUntil)
	if oldest := history.failures[0]; !oldest.IsZero() {
		hold(autoWaitLimit, oldest.Add(autoBackupLimitWindow))
	}
	if wait == "" {
		return "", time.Time{}
	}
	return wait, notBefore
}

func autoEvery(days int) time.Duration {
	return time.Duration(days)*24*time.Hour - autoSlack
}

// Without TZ, time.Local is the host's zone and has no IANA name.
func serverZone() string {
	if name := time.Local.String(); name != "Local" {
		return name
	}
	name, _ := time.Now().Zone()
	return name
}

func parseClock(value string) (int64, error) {
	clock, err := time.Parse("15:04", value)
	return int64(clock.Hour()*60 + clock.Minute()), err
}

func formatClock(minutes int64) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}
