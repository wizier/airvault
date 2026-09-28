package service

import (
	"context"
	"testing"

	"github.com/wizier/airvault/internal/model"
)

// A source with restore points and no registered phone is listed on its own,
// as orphaned.
func TestDeviceListShowsBackupOnlySources(t *testing.T) {
	svc, _ := newStoredService(t)
	ctx := context.Background()
	const source = "testphoneudid0005"
	row := model.Backup{ID: "cccccccc-0000-4000-8000-000000000003", SourceUDID: source, DeviceName: "Old iPhone", CreatedAt: 1_700_000_000}
	if err := svc.store.Backup.InsertSnapshot(ctx, row); err != nil {
		t.Fatal(err)
	}
	devices, err := svc.DeviceList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].UDID != source || devices[0].Name != "Old iPhone" || !devices[0].Orphaned || devices[0].RestorePoints != 1 {
		t.Fatalf("devices = %+v", devices)
	}
}
