package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/model"
)

// backupAt is a restore point named after its time, which is RFC 3339 or a
// date at 20:00 UTC.
func backupAt(t *testing.T, when string) model.Backup {
	t.Helper()
	at, err := time.Parse(time.RFC3339, when)
	if err != nil {
		var dateErr error
		if at, dateErr = time.Parse(time.DateOnly, when); dateErr != nil {
			t.Fatal(err)
		}
		at = at.Add(20 * time.Hour)
	}
	return model.Backup{ID: when, CreatedAt: at.Unix()}
}

// daily is a restore point a day from from back to to, newest first.
func daily(t *testing.T, from, to string) []model.Backup {
	t.Helper()
	first, err := time.Parse(time.DateOnly, from)
	if err != nil {
		t.Fatal(err)
	}
	last, err := time.Parse(time.DateOnly, to)
	if err != nil {
		t.Fatal(err)
	}
	var points []model.Backup
	for day := first; !day.Before(last); day = day.AddDate(0, 0, -1) {
		points = append(points, backupAt(t, day.Format(time.DateOnly)))
	}
	return points
}

func kept(points []model.Backup, remove []string) []string {
	var ids []string
	for _, point := range points {
		if !slices.Contains(remove, point.ID) {
			ids = append(ids, point.ID)
		}
	}
	return ids
}

func TestPrunable(t *testing.T) {
	moscow, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	with := func(points []model.Backup, id string, change func(*model.Backup)) []model.Backup {
		points = slices.Clone(points)
		change(&points[slices.IndexFunc(points, func(p model.Backup) bool { return p.ID == id })])
		return points
	}
	damaged := func(p *model.Backup) { p.Damage = "files_missing" }
	tests := []struct {
		name     string
		points   []model.Backup
		settings CleanupSettings
		open     string
		loc      *time.Location
		want     []string
	}{
		{"fewer days than kept", daily(t, "2026-10-05", "2026-10-03"),
			CleanupSettings{KeepDays: 7, Thin: ThinNone}, "", time.UTC,
			[]string{"2026-10-05", "2026-10-04", "2026-10-03"}},
		{"none keeps only the last days", daily(t, "2026-10-05", "2026-09-26"),
			CleanupSettings{KeepDays: 3, Thin: ThinNone}, "", time.UTC,
			[]string{"2026-10-05", "2026-10-04", "2026-10-03"}},
		{"the newest of each month", daily(t, "2026-10-05", "2026-07-01"),
			CleanupSettings{KeepDays: 3, Thin: ThinMonth}, "", time.UTC,
			[]string{"2026-10-05", "2026-10-04", "2026-10-03", "2026-09-30", "2026-08-31", "2026-07-31"}},
		{"the newest of each week from Monday", daily(t, "2026-10-05", "2026-09-10"),
			CleanupSettings{KeepDays: 1, Thin: ThinWeek}, "", time.UTC,
			[]string{"2026-10-05", "2026-10-04", "2026-09-27", "2026-09-20", "2026-09-13"}},
		{"a period whose newest is among the latest keeps no other", daily(t, "2026-10-05", "2026-09-28"),
			CleanupSettings{KeepDays: 2, Thin: ThinWeek}, "", time.UTC,
			[]string{"2026-10-05", "2026-10-04"}},
		{"days count back from the latest backup", []model.Backup{
			backupAt(t, "2026-10-05"), backupAt(t, "2026-10-02"), backupAt(t, "2026-09-29"), backupAt(t, "2026-09-26"),
		}, CleanupSettings{KeepDays: 7, Thin: ThinNone}, "", time.UTC,
			[]string{"2026-10-05", "2026-10-02", "2026-09-29"}},
		{"a phone that stopped backing up keeps its last days", daily(t, "2026-06-10", "2026-06-01"),
			CleanupSettings{KeepDays: 3, Thin: ThinNone}, "", time.UTC,
			[]string{"2026-06-10", "2026-06-09", "2026-06-08"}},
		{"damaged ones never go", with(daily(t, "2026-10-05", "2026-10-01"), "2026-10-03", damaged),
			CleanupSettings{KeepDays: 2, Thin: ThinNone}, "", time.UTC,
			[]string{"2026-10-05", "2026-10-04", "2026-10-03"}},
		{"a damaged newest leaves its period to the next", with(daily(t, "2026-10-02", "2026-09-28"), "2026-09-30", damaged),
			CleanupSettings{KeepDays: 2, Thin: ThinMonth}, "", time.UTC,
			[]string{"2026-10-02", "2026-10-01", "2026-09-30", "2026-09-29"}},
		{"a burst of manual backups pushes nothing out", append([]model.Backup{
			backupAt(t, "2026-10-05T12:20:00Z"), backupAt(t, "2026-10-05T12:10:00Z"), backupAt(t, "2026-10-05T12:00:00Z"),
		}, daily(t, "2026-10-04", "2026-10-01")...), CleanupSettings{KeepDays: 2, Thin: ThinNone}, "", time.UTC,
			[]string{"2026-10-05T12:20:00Z", "2026-10-05T12:10:00Z", "2026-10-05T12:00:00Z", "2026-10-04"}},
		{"a day past the latest thins like any other", []model.Backup{
			backupAt(t, "2026-10-05"), backupAt(t, "2026-10-04"),
			backupAt(t, "2026-09-30"), backupAt(t, "2026-09-30T12:00:00Z"), backupAt(t, "2026-09-29"),
		}, CleanupSettings{KeepDays: 2, Thin: ThinMonth}, "", time.UTC,
			[]string{"2026-10-05", "2026-10-04", "2026-09-30"}},
		{"open ones stay", daily(t, "2026-10-05", "2026-10-01"),
			CleanupSettings{KeepDays: 2, Thin: ThinNone}, "2026-10-01", time.UTC,
			[]string{"2026-10-05", "2026-10-04", "2026-10-01"}},
		{"periods are read in the server's zone", []model.Backup{
			backupAt(t, "2026-10-05"), backupAt(t, "2026-09-30T22:00:00Z"), backupAt(t, "2026-09-30T19:00:00Z"),
		}, CleanupSettings{KeepDays: 1, Thin: ThinMonth}, "", moscow,
			[]string{"2026-10-05", "2026-09-30T19:00:00Z"}},
	}
	for _, test := range tests {
		open := func(id string) bool { return id == test.open }
		got := kept(test.points, prunable(test.points, test.settings, open, test.loc))
		if !slices.Equal(got, test.want) {
			t.Errorf("%s: kept %v, want %v", test.name, got, test.want)
		}
	}
}

// A restore point thinning kept stays kept as days go by and backups pile up,
// bursts of manual ones included.
func TestPrunableNeverTakesBackWhatItKept(t *testing.T) {
	never := func(string) bool { return false }
	for _, thin := range []ThinPeriod{ThinWeek, ThinMonth} {
		settings := CleanupSettings{KeepDays: 7, Thin: thin}
		var points []model.Backup
		thinned := map[string]bool{}
		day := time.Date(2025, 12, 20, 0, 0, 0, 0, time.UTC)
		for n := range 400 {
			day = day.AddDate(0, 0, 1)
			backups := 1
			if n%5 == 0 {
				backups += n%9 + 1
			}
			for i := range backups {
				at := day.Add(time.Duration(8*60+30*i) * time.Minute)
				points = slices.Insert(points, 0, model.Backup{ID: at.Format(time.RFC3339), CreatedAt: at.Unix()})
				remove := prunable(points, settings, never, time.UTC)
				for _, id := range remove {
					if thinned[id] {
						t.Fatalf("%s: %s removed at %s after thinning kept it", thin, id, at.Format(time.RFC3339))
					}
				}
				points = slices.DeleteFunc(points, func(p model.Backup) bool { return slices.Contains(remove, p.ID) })
				// Past the last days, what stays was kept by thinning.
				since := time.Unix(points[0].CreatedAt, 0).UTC().Truncate(24*time.Hour).AddDate(0, 0, 1-settings.KeepDays)
				for _, point := range points {
					if time.Unix(point.CreatedAt, 0).Before(since) {
						thinned[point.ID] = true
					}
				}
			}
		}
	}
}

func TestCleanupSettingsValidation(t *testing.T) {
	invalid := []struct {
		settings CleanupSettings
		code     string
	}{
		{CleanupSettings{Enabled: true, KeepDays: 3, Thin: ThinMonth}, "invalid_cleanup_days"},
		{CleanupSettings{KeepDays: 0, Thin: ThinMonth}, "invalid_cleanup_days"},
		{CleanupSettings{KeepDays: 14, Thin: "day"}, "invalid_cleanup_thin"},
		{CleanupSettings{KeepDays: 14}, "invalid_cleanup_thin"},
	}
	for _, test := range invalid {
		_, err := cleanupRow(test.settings)
		if validation, ok := errors.AsType[*domain.ValidationError](err); !ok || validation.Code != test.code {
			t.Errorf("cleanupRow(%+v) = %v, want %s", test.settings, err, test.code)
		}
	}

	settings := CleanupSettings{Enabled: true, KeepDays: 30, Thin: ThinWeek}
	row, err := cleanupRow(settings)
	if err != nil {
		t.Fatal(err)
	}
	if back := cleanupSettingsOf(row); back != settings {
		t.Fatalf("round trip = %+v, want %+v", back, settings)
	}
}

// After a backup the phone's cleanup picks by the settings as they are then.
func TestCleanupAfterBackupReadsTheSettings(t *testing.T) {
	svc, _ := newStoredService(t)
	ctx := context.Background()
	const udid = "testphoneudid0007"
	if err := svc.store.Device.Upsert(ctx, &model.Device{UDID: udid, Name: "iPhone", Paired: true}); err != nil {
		t.Fatal(err)
	}
	points := daily(t, "2026-10-05", "2026-09-26")
	if remove := svc.cleanupAfterBackup(ctx, udid, points); len(remove) != 0 {
		t.Fatalf("cleanup that is off removes %v", remove)
	}
	if err := svc.store.Device.SetCleanup(ctx, udid, model.Cleanup{Enabled: true, KeepDays: 7, Thin: string(ThinNone)}); err != nil {
		t.Fatal(err)
	}
	if got := kept(points, svc.cleanupAfterBackup(ctx, udid, points)); len(got) != 7 {
		t.Fatalf("kept %v, want the last 7 days", got)
	}
}
