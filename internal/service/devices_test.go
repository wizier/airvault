package service

import (
	"context"
	"testing"

	"github.com/wizier/airvault/internal/iosbackup"
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

// Every source's restore points are listed together, the latest first, each
// with its source and whether it is held open for browsing.
func TestRestorePointsListEverySource(t *testing.T) {
	svc, _ := newStoredService(t)
	svc.unlocked.changed = func(string) {}
	defer svc.unlocked.closeIf(func(*unlockedBackup) bool { return true })
	ctx := t.Context()
	rows := []model.Backup{
		{ID: "cccccccc-0000-4000-8000-000000000003", SourceUDID: "testphoneudid0005", Encrypted: true, CreatedAt: 1},
		{ID: "cccccccc-0000-4000-8000-000000000004", SourceUDID: "testphoneudid0006", Encrypted: true, CreatedAt: 2},
	}
	for _, row := range rows {
		if err := svc.store.Backup.InsertSnapshot(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	svc.unlocked.put(rows[0].ID, rows[0].SourceUDID, &iosbackup.Contents{})

	points, err := svc.RestorePoints(ctx)
	if err != nil || len(points) != 2 {
		t.Fatalf("RestorePoints = %+v, %v", points, err)
	}
	latest, older := points[0], points[1]
	if latest.SnapshotID != rows[1].ID || latest.UDID != rows[1].SourceUDID || latest.Unlocked {
		t.Errorf("latest = %+v", latest)
	}
	if older.SnapshotID != rows[0].ID || older.UDID != rows[0].SourceUDID || !older.Unlocked {
		t.Errorf("older = %+v", older)
	}
}
