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
