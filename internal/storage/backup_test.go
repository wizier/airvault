package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/wizier/airvault/internal/model"
)

func insertTestSnapshot(t *testing.T, store *Store, id string) {
	t.Helper()
	if err := store.Backup.InsertSnapshot(context.Background(), model.Backup{
		ID:         id,
		SourceUDID: testSource,
		IOSVersion: "18.0",
		CreatedAt:  1_700_000_000,
	}); err != nil {
		t.Fatal(err)
	}
}

// No cached footprint may outlive the source's snapshots, nor come back with
// a snapshot of the same source admitted later.
func TestNoCachedFootprintOutlivesItsSnapshots(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	const id = "eeeeeeee-0000-4000-8000-00000000000c"
	insertTestSnapshot(t, store, id)
	if err := store.Backup.SetSourceFootprint(ctx, testSource, 4096); err != nil {
		t.Fatal(err)
	}

	if err := store.Backup.DeleteSnapshot(ctx, testSource, id); err != nil {
		t.Fatal(err)
	}
	if err := store.Backup.DropUnreferencedFootprints(ctx); err != nil {
		t.Fatal(err)
	}
	insertTestSnapshot(t, store, id)
	summary, err := store.Backup.SummaryBySource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary[testSource].DiskBytes; got != nil {
		t.Fatalf("footprint = %d, want unknown", *got)
	}
}

// A source's latest backup is its newest restorable one; damaged ones still
// count as restore points.
func TestSummaryLatestSkipsDamage(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	for i, damage := range []string{"", "files_missing"} {
		if err := store.Backup.InsertSnapshot(ctx, model.Backup{ID: fmt.Sprintf("eeeeeeee-0000-4000-8000-00000000000%d", i),
			SourceUDID: testSource, CreatedAt: int64(1_700_000_000 + i), Damage: damage}); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := store.Backup.SummaryBySource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := summary[testSource]; s.RestorePoints != 2 || s.LatestRestorable == nil || *s.LatestRestorable != 1_700_000_000 {
		t.Fatalf("summary = %+v", s)
	}
}

// The summary's last check is the source's newest one, and its damage counts only
// its own damaged restore points.
func TestSummaryVerifiedAndDamage(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	const otherSource = "testphoneudid0002"
	rows := []model.Backup{
		{ID: "eeeeeeee-0000-4000-8000-000000000001", SourceUDID: testSource, CreatedAt: 1_700_000_000},
		{ID: "eeeeeeee-0000-4000-8000-000000000002", SourceUDID: testSource, CreatedAt: 1_700_000_001, Damage: "files_missing"},
		{ID: "eeeeeeee-0000-4000-8000-000000000003", SourceUDID: otherSource, CreatedAt: 1_700_000_002},
	}
	for _, row := range rows {
		if err := store.Backup.InsertSnapshot(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	const checkedAt = int64(1_700_000_100)
	if err := store.Backup.MarkVerified(ctx, testSource, checkedAt); err != nil {
		t.Fatal(err)
	}
	// A backup published after the check has none of its own.
	if err := store.Backup.InsertSnapshot(ctx, model.Backup{ID: "eeeeeeee-0000-4000-8000-000000000004",
		SourceUDID: testSource, CreatedAt: 1_700_000_200}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Backup.SummaryBySource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checked := summary[testSource]
	if checked.LatestVerified == nil || *checked.LatestVerified != checkedAt || checked.DamagedPoints != 1 {
		t.Fatalf("checked source summary = %+v", checked)
	}
	if other := summary[otherSource]; other.LatestVerified != nil || other.DamagedPoints != 0 {
		t.Fatalf("other source summary = %+v", other)
	}
}
