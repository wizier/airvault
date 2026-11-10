package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/storage"
)

func validAutomaticTasks() AutomaticTasks {
	return AutomaticTasks{
		AutoBackup: ScheduleSettings{EveryDays: 1},
		Cleanup:    CleanupSettings{KeepDays: 14, Thin: ThinMonth},
		AutoVerify: ScheduleSettings{EveryDays: 7},
	}
}

func TestAutomaticTasksValidation(t *testing.T) {
	window := func(start, end string) *AutoWindow { return &AutoWindow{Start: start, End: end} }
	invalid := []struct {
		name   string
		change func(*AutomaticTasks)
		code   domain.Code
	}{
		{"a check's interval for a backup", func(tasks *AutomaticTasks) { tasks.AutoBackup.EveryDays = 14 },
			"invalid_auto_backup_interval"},
		{"no backup section", func(tasks *AutomaticTasks) { tasks.AutoBackup = ScheduleSettings{} },
			"invalid_auto_backup_interval"},
		{"a backup's interval for a check", func(tasks *AutomaticTasks) { tasks.AutoVerify.EveryDays = 1 },
			"invalid_auto_verify_interval"},
		{"no check section", func(tasks *AutomaticTasks) { tasks.AutoVerify = ScheduleSettings{} },
			"invalid_auto_verify_interval"},
		{"no cleanup section", func(tasks *AutomaticTasks) { tasks.Cleanup = CleanupSettings{} },
			"invalid_cleanup_days"},
		{"a cleanup period off the list", func(tasks *AutomaticTasks) { tasks.Cleanup.Thin = "year" },
			"invalid_cleanup_thin"},
		{"an empty backup window", func(tasks *AutomaticTasks) { tasks.AutoBackup.Window = window("19:00", "19:00") },
			"invalid_auto_window"},
		{"a check window past midnight", func(tasks *AutomaticTasks) { tasks.AutoVerify.Window = window("24:00", "02:00") },
			"invalid_auto_window"},
		{"a window not in HH:MM", func(tasks *AutomaticTasks) { tasks.AutoVerify.Window = window("02:00", "7pm") },
			"invalid_auto_window"},
	}
	for _, test := range invalid {
		tasks := validAutomaticTasks()
		test.change(&tasks)
		_, err := automaticTasksRow(tasks)
		if validation, ok := errors.AsType[*domain.ValidationError](err); !ok || validation.Code != test.code {
			t.Errorf("%s: %v, want %s", test.name, err, test.code)
		}
	}

	tasks := validAutomaticTasks()
	tasks.AutoBackup = ScheduleSettings{Enabled: true, EveryDays: 3, Window: window("22:30", "01:15")}
	row, err := automaticTasksRow(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if *row.AutoBackup.WindowStart != 22*60+30 || *row.AutoBackup.WindowEnd != 75 {
		t.Fatalf("stored window = %d–%d", *row.AutoBackup.WindowStart, *row.AutoBackup.WindowEnd)
	}
	back := scheduleSettingsOf(row.AutoBackup)
	if !back.Enabled || back.EveryDays != 3 || *back.Window != *tasks.AutoBackup.Window {
		t.Fatalf("round trip = %+v, want %+v", back, tasks.AutoBackup)
	}
	if row.AutoVerify.WindowStart != nil || row.AutoVerify.WindowEnd != nil {
		t.Fatalf("no window = %+v, want any time", row.AutoVerify)
	}
}

// What is saved is what the device list shows, each section in its own place; a
// rejected save changes nothing, even in its valid sections.
func TestSetAutomaticTasks(t *testing.T) {
	ctx := context.Background()
	const phone = "testphoneudid0007"
	svc, addPhone := newCheckableService(t)
	addPhone(phone, model.Schedule{Days: 7})

	tasks := AutomaticTasks{
		AutoBackup: ScheduleSettings{Enabled: true, EveryDays: 3, Window: &AutoWindow{Start: "19:00", End: "23:00"}},
		Cleanup:    CleanupSettings{Enabled: true, KeepDays: 30, Thin: ThinWeek},
		AutoVerify: ScheduleSettings{Enabled: true, EveryDays: 30},
	}
	rejected := tasks
	rejected.AutoVerify.EveryDays = 2
	err := svc.SetAutomaticTasks(ctx, phone, rejected)
	if _, ok := errors.AsType[*domain.ValidationError](err); !ok {
		t.Fatalf("rejected save = %v, want a validation error", err)
	}
	overview := func() DeviceOverview {
		t.Helper()
		devices, err := svc.DeviceList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(devices) != 1 || devices[0].UDID != phone {
			t.Fatalf("devices = %+v, want %s alone", devices, phone)
		}
		return devices[0]
	}
	if got := overview(); got.AutoBackup.Enabled || got.Cleanup.Enabled {
		t.Fatalf("a rejected save changed the settings: %+v %+v", got.AutoBackup, got.Cleanup)
	}

	if err := svc.SetAutomaticTasks(ctx, phone, tasks); err != nil {
		t.Fatal(err)
	}
	got := overview()
	if backup := got.AutoBackup; !backup.Enabled || backup.EveryDays != 3 || *backup.Window != *tasks.AutoBackup.Window {
		t.Fatalf("auto backup = %+v, want %+v", backup, tasks.AutoBackup)
	}
	if *got.Cleanup != tasks.Cleanup {
		t.Fatalf("cleanup = %+v, want %+v", *got.Cleanup, tasks.Cleanup)
	}
	if check := got.AutoVerify; !check.Enabled || check.EveryDays != 30 || check.Window != nil {
		t.Fatalf("auto verify = %+v, want %+v", check, tasks.AutoVerify)
	}

	if err := svc.SetAutomaticTasks(ctx, "testphoneudid0099", tasks); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("save for an unknown device = %v, want not found", err)
	}
}

// A backup's schedule counts from the latest restorable one, not the last check.
func TestScheduleWaitCountsFromTheBackup(t *testing.T) {
	now := time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC)
	backups := storage.SourceSummary{RestorePoints: 1, LatestRestorable: new(now.Unix())}
	registry := newRunRegistry(events.New())
	wait, _ := registry.scheduleWait(runKindBackup, "phone", model.Schedule{Days: 7}, backups, now)
	if wait != autoWaitSchedule {
		t.Errorf("wait after a fresh backup = %q, want %q", wait, autoWaitSchedule)
	}
}

// The list shows each task its own wait, and none for a task that is off.
func TestDeviceListShowsEachTasksWait(t *testing.T) {
	ctx := context.Background()
	const phone = "testphoneudid0007"
	svc, addPhone := newCheckableService(t)
	addPhone(phone, model.Schedule{Days: 7})
	views := func(on bool) (backup, check *ScheduleView) {
		t.Helper()
		schedule := model.Schedule{Enabled: on, Days: 7}
		tasks := model.AutomaticTasks{AutoBackup: schedule, AutoVerify: schedule}
		if err := svc.store.Device.SetAutomaticTasks(ctx, phone, tasks); err != nil {
			t.Fatal(err)
		}
		devices, err := svc.DeviceList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return devices[0].AutoBackup, devices[0].AutoVerify
	}
	now := time.Now()
	run := &runReservation{udid: phone, auto: true}

	svc.runs.recordAutoVerify(run, runStateFailed, now)
	backup, check := views(true)
	if backup.Wait != "" || backup.NotBefore != nil {
		t.Errorf("a paused check holds the backup: %+v", backup)
	}
	if check.Wait != autoWaitPaused || check.NotBefore == nil || !check.NotBefore.Equal(now.Add(autoEvery(1))) {
		t.Errorf("paused check = %+v", check)
	}

	backup, check = views(false)
	if backup.Wait != "" || backup.NotBefore != nil || check.Wait != "" || check.NotBefore != nil {
		t.Errorf("tasks that are off show a wait: %+v %+v", backup, check)
	}
}

// Each trigger reads its own task's schedule and pause.
func TestAutoBackupDueReadsItsSchedule(t *testing.T) {
	ctx := context.Background()
	const phone = "testphoneudid0007"
	svc, addPhone := newCheckableService(t)
	addPhone(phone, model.Schedule{Enabled: true, Days: 7})
	due := func() bool {
		t.Helper()
		due, err := svc.autoBackupDue(ctx, phone)
		if err != nil {
			t.Fatal(err)
		}
		return due
	}
	if due() {
		t.Fatal("a check that is on started a backup")
	}
	tasks := model.AutomaticTasks{AutoBackup: model.Schedule{Enabled: true, Days: 1}}
	if err := svc.store.Device.SetAutomaticTasks(ctx, phone, tasks); err != nil {
		t.Fatal(err)
	}
	if !due() {
		t.Fatal("a backup that is on and overdue did not start")
	}
	run := &runReservation{udid: phone, auto: true}
	svc.runs.recordAutoVerify(run, runStateFailed, time.Now())
	if !due() {
		t.Fatal("a paused check held a backup")
	}
	svc.runs.recordAutoBackup(run, runStateFailed, "device_timeout", time.Now())
	if due() {
		t.Fatal("a paused backup started")
	}
}
