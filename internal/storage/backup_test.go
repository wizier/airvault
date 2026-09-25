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

// Re-adopting a source starts its size from nothing, so no cached footprint may
// outlive the source's snapshots. A delta then needs a measured base: applied to
// an unknown size it must leave it unknown, not seed one short by everything
// already on disk.
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
	insertTestSnapshot(t, store, id)
	if err := store.Backup.AddSourceFootprint(ctx, footprintTestSource, 512); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Backup.SummaryBySource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary[footprintTestSource].DiskBytes; got != nil {
		t.Fatalf("footprint = %d, want unknown", *got)
	}
}
