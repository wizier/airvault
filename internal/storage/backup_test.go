package storage

import (
	"context"
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

// No cached footprint may outlive the source's snapshots. A delta needs a
// measured base: applied to an unknown size it must leave it unknown, not seed
// one short by everything already on disk.
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
	if err := store.Backup.AddSourceFootprint(ctx, testSource, 512); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Backup.SummaryBySource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary[testSource].DiskBytes; got != nil {
		t.Fatalf("footprint = %d, want unknown", *got)
	}
}
