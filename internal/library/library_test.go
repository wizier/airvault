package library

import (
	"archive/tar"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/model"
)

// An export is named after the phone and packs a "<UDID>-<date>" folder that
// can't merge into an existing backup of the same phone.
func TestExportNamesFileAndFolder(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0007"
	publishFixture(t, lib, root, source, testSnapshot)
	reconcile(t, lib)
	view, err := lib.objects.OpenSnapshot(source, testSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Unix(view.CreatedUnix(), 0)

	export, err := lib.Export(context.Background(), testSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer export.Close()
	if want := "AirVault-Test iPhone-" + created.Format("2006-01-02-1504") + ".tar"; export.Name != want {
		t.Fatalf("name = %q, want %q", export.Name, want)
	}
	header, err := tar.NewReader(export).Next()
	if err != nil {
		t.Fatal(err)
	}
	if want := source + "-" + created.Format("20060102-150405") + "/"; header.Name != want {
		t.Fatalf("root folder = %q, want %q", header.Name, want)
	}
}

func TestExportOfAnUnknownSnapshotIsNotFound(t *testing.T) {
	lib, _ := newTestLibrary(t)
	if _, err := lib.Export(context.Background(), testSnapshot); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}

// Publish lists the restore point with the run's facts and grows the
// footprint by what the backup pooled plus its manifest.
func TestPublishListsTheRestorePoint(t *testing.T) {
	lib, _ := newTestLibrary(t)
	const source = "testphoneudid0061"
	ctx := context.Background()
	staged := sealFiles(t, lib, source, testSnapshot, fixtureFiles)
	row, err := Project(&staged.View)
	if err != nil {
		t.Fatal(err)
	}
	started, transferred := int64(1_700_000_000), int64(4096)
	row.StartedAt, row.TransferredBytes = &started, &transferred
	if err := lib.Publish(ctx, staged, row, 100); err != nil {
		t.Fatal(err)
	}
	listed, err := lib.catalog.Backup.Get(ctx, testSnapshot)
	if err != nil || *listed.StartedAt != started || *listed.TransferredBytes != transferred || listed.DeviceName != "Test iPhone" {
		t.Fatalf("catalog row = %+v, %v", listed, err)
	}
	manifest, err := lib.objects.SnapshotManifestBytes(source, testSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceFootprint(t, lib, source); got == nil || *got != 100+manifest {
		t.Fatalf("footprint = %v, want %d", got, 100+manifest)
	}
	if base, err := lib.LatestBase(ctx, source); err != nil || base == nil || base.ID() != testSnapshot {
		t.Fatalf("LatestBase = %v, %v", base, err)
	}
}

// A discarded snapshot leaves nothing: not its staging, not what it pooled.
func TestDiscardLeavesNothing(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0062"
	sealFiles(t, lib, source, testSnapshot, fixtureFiles)
	if err := lib.Discard(context.Background(), source, testSnapshot); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, filepath.Join(root, source))
	requireNotCataloged(t, lib, testSnapshot)
}

func TestOpenTellsMissingFromIncomplete(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0063"
	const incompleteID = "dddddddd-0000-4000-8000-000000000004"
	ctx := context.Background()
	publishFixture(t, lib, root, source, testSnapshot)
	reconcile(t, lib)
	publishFiles(t, lib, root, source, incompleteID, incompleteFiles())
	// Reconcile would never admit it; a catalog row can still outlive a
	// backup's completeness, as after bit rot.
	if err := lib.catalog.Backup.InsertSnapshot(ctx, model.Backup{ID: incompleteID, SourceUDID: source, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	if backup, err := lib.Open(ctx, testSnapshot); err != nil || backup.Info.DeviceName != "Test iPhone" {
		t.Fatalf("Open = %+v, %v", backup, err)
	}
	if _, err := lib.Open(ctx, incompleteID); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("incomplete backup: %v, want ErrIncomplete", err)
	}
	var validation *domain.ValidationError
	if _, err := lib.Open(ctx, "eeeeeeee-0000-4000-8000-000000000005"); !errors.As(err, &validation) || validation.Code != "snapshot_not_found" {
		t.Fatalf("unknown snapshot: %v, want snapshot_not_found", err)
	}
}
