package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/model"
)

// Discovery upserts a device's row every time it reappears; it must never
// reset the automatic-backup and cleanup settings stored on it.
func TestDiscoveryKeepsDeviceSettings(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	device := model.Device{UDID: testSource, Name: "iPhone", Paired: true}
	if err := store.Device.Upsert(ctx, &device); err != nil {
		t.Fatal(err)
	}
	start, end := int64(19*60), int64(23*60)
	settings := model.AutoBackup{Enabled: true, Days: 3, WindowStart: &start, WindowEnd: &end}
	if err := store.Device.SetAutoBackup(ctx, device.UDID, settings); err != nil {
		t.Fatal(err)
	}
	cleanup := model.Cleanup{Enabled: true, KeepDays: 30, Thin: "week"}
	if err := store.Device.SetCleanup(ctx, device.UDID, cleanup); err != nil {
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
	if !got.Enabled || got.Days != 3 || *got.WindowStart != start || *got.WindowEnd != end {
		t.Fatalf("settings after discovery = %+v, want %+v", got, settings)
	}
	if stored.Cleanup != cleanup {
		t.Fatalf("cleanup after discovery = %+v, want %+v", stored.Cleanup, cleanup)
	}
}

// A device starts with cleanup off, set to its defaults for when it is turned on.
func TestCleanupIsOffByDefault(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.Device.Upsert(ctx, &model.Device{UDID: testSource, Name: "iPhone"}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Device.GetByUDID(ctx, testSource)
	if err != nil {
		t.Fatal(err)
	}
	if want := (model.Cleanup{KeepDays: 14, Thin: "month"}); stored.Cleanup != want {
		t.Fatalf("cleanup = %+v, want %+v", stored.Cleanup, want)
	}
	if err := store.Device.SetCleanup(ctx, "testphoneudid0099", stored.Cleanup); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cleanup of an unknown device = %v, want not found", err)
	}
}
