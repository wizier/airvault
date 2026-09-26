package storage

import (
	"context"
	"testing"

	"github.com/wizier/airvault/internal/model"
)

// Discovery upserts a device's row every time it reappears; it must never
// reset the automatic-backup settings stored on it.
func TestDiscoveryKeepsAutoBackupSettings(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	device := model.Device{UDID: footprintTestSource, Name: "iPhone", Paired: true}
	if err := store.Device.Upsert(ctx, &device); err != nil {
		t.Fatal(err)
	}
	start, end := int64(19*60), int64(23*60)
	settings := model.AutoBackup{Enabled: true, Days: 3, WindowStart: &start, WindowEnd: &end,
		TimeZone: "Europe/Moscow"}
	if err := store.Device.SetAutoBackup(ctx, device.UDID, settings); err != nil {
		t.Fatal(err)
	}

	device.Name = "iPhone renamed"
	if err := store.Device.Upsert(ctx, &device); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Device.GetByUDID(ctx, device.UDID)
	if err != nil {
		t.Fatal(err)
	}
	got := stored.AutoBackup
	if !got.Enabled || got.Days != 3 || *got.WindowStart != start || *got.WindowEnd != end ||
		got.TimeZone != settings.TimeZone {
		t.Fatalf("settings after discovery = %+v, want %+v", got, settings)
	}
}

// A source without restore points has no latest one: the automatic trigger
// reads nil as "the first backup is still manual".
func TestLatestCreatedWithoutBackups(t *testing.T) {
	latest, err := newTestStore(t).Backup.LatestCreated(context.Background(), footprintTestSource)
	if err != nil || latest != nil {
		t.Fatalf("LatestCreated = %v, %v; want nil", latest, err)
	}
}
