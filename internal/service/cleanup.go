package service

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/model"
)

// Cleanup thins a phone's old restore points after each of its backups: those
// of its last days stay as they are, of older ones one per week or month, or
// none.

// ThinPeriod is how often an older restore point stays.
type ThinPeriod string

const (
	ThinWeek  ThinPeriod = "week"
	ThinMonth ThinPeriod = "month"
	ThinNone  ThinPeriod = "none"
)

var cleanupDayPresets = []int{7, 14, 30, 60}

type CleanupSettings struct {
	Enabled bool `json:"enabled"`
	// KeepDays counts calendar days back from the latest backup, not from now:
	// a phone that stopped backing up keeps its last days.
	KeepDays int        `json:"keepDays"`
	Thin     ThinPeriod `json:"thin"`
}

// A calendar week or month.
type period struct{ year, number int }

// periodOf is the period of thin that t falls in; false when thin keeps none.
// Weeks start on Monday.
func periodOf(t time.Time, thin ThinPeriod) (period, bool) {
	switch thin {
	case ThinWeek:
		year, week := t.ISOWeek()
		return period{year, week}, true
	case ThinMonth:
		return period{t.Year(), int(t.Month())}, true
	}
	return period{}, false
}

// prunable picks what cleanup removes from points, newest first as the catalog
// lists them. Only whole restore points count: those of the last KeepDays days
// up to the newest one's day stay, and so does the newest of each period, which
// never changes once the period is over. Days are read in loc. Damaged and open
// ones are never picked.
func prunable(points []model.Backup, settings CleanupSettings, open func(id string) bool, loc *time.Location) []string {
	remove := []string{}
	var since time.Time
	seen := map[period]bool{}
	for _, point := range points {
		if point.Damage != "" {
			continue
		}
		at := time.Unix(point.CreatedAt, 0).In(loc)
		if since.IsZero() {
			year, month, day := at.Date()
			since = time.Date(year, month, day-settings.KeepDays+1, 0, 0, 0, 0, loc)
		}
		key, thinned := periodOf(at, settings.Thin)
		newest := thinned && !seen[key]
		seen[key] = true
		if at.Before(since) && !newest && !open(point.ID) {
			remove = append(remove, point.ID)
		}
	}
	return remove
}

// SetCleanup stores the phone's cleanup settings; its next backup applies them.
func (s *Service) SetCleanup(ctx context.Context, udid string, settings CleanupSettings) error {
	row, err := cleanupRow(settings)
	if err != nil {
		return err
	}
	if err := s.store.Device.SetCleanup(ctx, udid, row); err != nil {
		return err
	}
	s.bus.Emit(deviceUpdated(udid))
	return nil
}

// PlanCleanup lists the restore points settings would remove now, enabled or
// not. It reads the catalog alone, so it answers at once.
func (s *Service) PlanCleanup(ctx context.Context, udid string, settings CleanupSettings) ([]string, error) {
	if _, err := cleanupRow(settings); err != nil {
		return nil, err
	}
	points, err := s.library.RestorePoints(ctx, udid)
	if err != nil {
		return nil, err
	}
	return prunable(points, settings, s.unlocked.has, time.Local), nil
}

// cleanupAfterBackup picks what the phone's cleanup removes once a backup is
// published. The settings are read then, so ones saved during the backup apply.
func (s *Service) cleanupAfterBackup(ctx context.Context, udid string, points []model.Backup) []string {
	device, err := s.store.Device.GetByUDID(ctx, udid)
	if err != nil {
		slog.WarnContext(ctx, "cleanup: deferred to the next backup", "udid", udid, "error", err)
		return nil
	}
	if !device.Cleanup.Enabled {
		return nil
	}
	remove := prunable(points, cleanupSettingsOf(device.Cleanup), s.unlocked.has, time.Local)
	s.unlocked.closeIf(func(b *unlockedBackup) bool { return slices.Contains(remove, b.snapshotID) })
	return remove
}

func cleanupRow(settings CleanupSettings) (model.Cleanup, error) {
	if !slices.Contains(cleanupDayPresets, settings.KeepDays) {
		return model.Cleanup{}, &domain.ValidationError{Code: "invalid_cleanup_days",
			Message: fmt.Sprintf("cleanup keeps every backup of one of %v last days", cleanupDayPresets)}
	}
	switch settings.Thin {
	case ThinWeek, ThinMonth, ThinNone:
	default:
		return model.Cleanup{}, &domain.ValidationError{Code: "invalid_cleanup_thin",
			Message: "older backups thin to one a week, one a month or none"}
	}
	return model.Cleanup{Enabled: settings.Enabled, KeepDays: settings.KeepDays, Thin: string(settings.Thin)}, nil
}

func cleanupSettingsOf(stored model.Cleanup) CleanupSettings {
	return CleanupSettings{Enabled: stored.Enabled, KeepDays: stored.KeepDays, Thin: ThinPeriod(stored.Thin)}
}
