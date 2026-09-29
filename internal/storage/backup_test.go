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
