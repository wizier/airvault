package storage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"
	airvault "github.com/wizier/airvault"
	"github.com/wizier/airvault/internal/model"
	_ "modernc.org/sqlite"
)

const footprintTestSource = "testphoneudid0031"

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sqlx.Open("sqlite", filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	goose.SetBaseFS(airvault.MigrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(db.DB, "migrations"); err != nil {
		t.Fatal(err)
	}
	return NewStore(db)
}

func insertTestSnapshot(t *testing.T, store *Store, id string) {
	t.Helper()
	if err := store.Backup.InsertSnapshot(context.Background(), model.Backup{
		ID:         id,
		SourceUDID: footprintTestSource,
		IOSVersion: "18.0",
		CreatedAt:  1_700_000_000,
	}); err != nil {
		t.Fatal(err)
	}
}

func cachedFootprint(t *testing.T, store *Store) *int64 {
	t.Helper()
	summary, err := store.Backup.SummaryBySource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return summary[footprintTestSource].DiskBytes
}

// A delta needs a measured base. Applied to a source with no cached size it must
// leave the size unknown, not seed one that is short by everything already there.
func TestAddSourceFootprintLeavesAnUnknownSizeUnknown(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	insertTestSnapshot(t, store, "eeeeeeee-0000-4000-8000-00000000000a")

	if err := store.Backup.AddSourceFootprint(ctx, footprintTestSource, 512); err != nil {
		t.Fatal(err)
	}
	if got := cachedFootprint(t, store); got != nil {
		t.Fatalf("footprint = %d, want unknown", *got)
	}
}

func TestNoCachedFootprintOutlivesItsSnapshots(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	const id = "eeeeeeee-0000-4000-8000-00000000000c"
	insertTestSnapshot(t, store, id)
	if err := store.Backup.SetSourceFootprint(ctx, footprintTestSource, 4096); err != nil {
		t.Fatal(err)
	}

	if err := store.Backup.DeleteSnapshot(ctx, footprintTestSource, id); err != nil {
		t.Fatal(err)
	}
	if err := store.Backup.DropUnreferencedFootprints(ctx); err != nil {
		t.Fatal(err)
	}
	// Re-adopting the source starts from nothing, so a leftover row would shift a
	// size that no longer describes anything.
	insertTestSnapshot(t, store, id)
	if err := store.Backup.AddSourceFootprint(ctx, footprintTestSource, 512); err != nil {
		t.Fatal(err)
	}
	if got := cachedFootprint(t, store); got != nil {
		t.Fatalf("footprint = %d, want unknown", *got)
	}
}
