package storage

import (
	"context"
	"testing"

	"github.com/wizier/airvault/internal/model"
)

func upsertTestDevice(t *testing.T, store *Store) {
	t.Helper()
	device := model.Device{UDID: testSource, Name: "iPhone", Paired: true}
	if err := store.Device.Upsert(t.Context(), &device); err != nil {
		t.Fatal(err)
	}
}

func storedDevice(t *testing.T, store *Store) *model.Device {
	t.Helper()
	stored, err := store.Device.GetByUDID(t.Context(), testSource)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func isDefaultAutoBackup(got model.Schedule) bool {
	return !got.Enabled && got.Days == 1 && got.WindowStart == nil && got.WindowEnd == nil
}

func isDefaultAutoVerify(got model.Schedule) bool {
	return !got.Enabled && got.Days == 7 && got.WindowStart != nil && *got.WindowStart == 2*60 &&
		got.WindowEnd != nil && *got.WindowEnd == 6*60
}

// Settings survive discovery and go with the device: a phone paired again starts over.
func TestDeviceSettingsLifeCycle(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	upsertTestDevice(t, store)
	tasks := model.AutomaticTasks{
		Cleanup:    model.Cleanup{Enabled: true, KeepDays: 30, Thin: "week"},
		AutoBackup: model.Schedule{Enabled: true, Days: 3},
	}
	if err := store.Device.SetAutomaticTasks(ctx, testSource, tasks); err != nil {
		t.Fatal(err)
	}
	renamed := model.Device{UDID: testSource, Name: "iPhone renamed", Paired: true}
	if err := store.Device.Upsert(ctx, &renamed); err != nil {
		t.Fatal(err)
	}
	stored := storedDevice(t, store)
	if stored.Cleanup != tasks.Cleanup || stored.AutoBackup != tasks.AutoBackup {
		t.Fatalf("after discovery = %+v, want %+v", stored.AutomaticTasks, tasks)
	}
	if err := store.Device.Delete(ctx, testSource); err != nil {
		t.Fatal(err)
	}
	upsertTestDevice(t, store)
	if got := storedDevice(t, store).AutoBackup; !isDefaultAutoBackup(got) {
		t.Fatalf("auto backup of a phone paired again = %+v, want the default", got)
	}
}

// List reads each device its own schedules, and the default where it has none.
func TestDeviceListReadsEachDevicesSchedules(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	upsertTestDevice(t, store)
	const other = "testphoneudid0098"
	if err := store.Device.Upsert(ctx, &model.Device{UDID: other, Name: "iPad"}); err != nil {
		t.Fatal(err)
	}
	tasks := model.AutomaticTasks{
		AutoBackup: model.Schedule{Enabled: true, Days: 3},
		AutoVerify: model.Schedule{Enabled: true, Days: 30},
	}
	if err := store.Device.SetAutomaticTasks(ctx, testSource, tasks); err != nil {
		t.Fatal(err)
	}
	devices, err := store.Device.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("devices = %+v, want 2", devices)
	}
	ipad, iphone := devices[0], devices[1] // listed by name
	if iphone.AutoBackup != tasks.AutoBackup || iphone.AutoVerify != tasks.AutoVerify {
		t.Errorf("saved: %+v %+v, want %+v", iphone.AutoBackup, iphone.AutoVerify, tasks)
	}
	if !isDefaultAutoBackup(ipad.AutoBackup) || !isDefaultAutoVerify(ipad.AutoVerify) {
		t.Errorf("never saved: %+v %+v, want the defaults", ipad.AutoBackup, ipad.AutoVerify)
	}
}

// A failure in the last write leaves what came before it as it was.
func TestSetAutomaticTasksSavesAllOrNothing(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	upsertTestDevice(t, store)
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER refuse_verify BEFORE INSERT ON device_schedules WHEN NEW.task = 'verify'
		BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	tasks := model.AutomaticTasks{
		Cleanup:    model.Cleanup{Enabled: true, KeepDays: 30, Thin: "week"},
		AutoBackup: model.Schedule{Enabled: true, Days: 3},
		AutoVerify: model.Schedule{Enabled: true, Days: 30},
	}
	if err := store.Device.SetAutomaticTasks(ctx, testSource, tasks); err == nil {
		t.Fatal("the refused write went through")
	}
	stored := storedDevice(t, store)
	defaultCleanup := model.Cleanup{KeepDays: 14, Thin: "month"}
	if stored.Cleanup != defaultCleanup || !isDefaultAutoBackup(stored.AutoBackup) || !isDefaultAutoVerify(stored.AutoVerify) {
		t.Fatalf("a failed save left %+v", stored.AutomaticTasks)
	}
}
