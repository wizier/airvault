package library

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wizier/airvault/internal/objectstore"
)

// An attempt cut short before publication is gone after startup; the next
// backup starts full.
func TestReconcileDiscardsAnUnpublishedAttempt(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0003"
	stagingDir := writeUnpublishedStaging(t, root, source)
	reconcile(t, lib)

	if base, err := lib.LatestBase(context.Background(), source); base != nil || err != nil {
		t.Fatalf("LatestBase = %v, %v; want none", base, err)
	}
	requireNotCataloged(t, lib, testSnapshot)
	requireAbsent(t, stagingDir)
}

// An attempt that crashed past its manifest's rename is a restore point, and
// the base of the next backup.
func TestReconcileRecoversAPublishedAttempt(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0004"
	publishedPaths := publishFixture(t, lib, root, source, testSnapshot)
	stagingDir := filepath.Join(root, source, "staging", testSnapshot)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reconcile(t, lib)

	if base, err := lib.LatestBase(context.Background(), source); err != nil || base == nil || base.ID() != testSnapshot {
		t.Fatalf("LatestBase = %v, %v; want %s", base, err, testSnapshot)
	}
	requireAbsent(t, stagingDir)
	recovered, err := lib.catalog.Backup.Get(context.Background(), testSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.CreatedAt == 0 {
		t.Fatalf("recovered snapshot has no creation time: %+v", recovered)
	}
	requirePresent(t, publishedPaths...)
}

// Reconcile rebuilds the catalog from manifests, returns every source for the
// background scrub, keeps a surviving source's footprint across restarts and
// forgets a snapshot whose manifest disappeared.
func TestReconcileKeepsCatalogInStepWithManifests(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0005"
	paths := publishFixture(t, lib, root, source, testSnapshot)
	manifestPath := paths[len(paths)-1]

	if sources := reconcile(t, lib); len(sources) != 1 || sources[0] != source {
		t.Fatalf("reconcile sources = %v, want [%s]", sources, source)
	}
	backup, err := lib.catalog.Backup.Get(context.Background(), testSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	view, err := lib.objects.OpenSnapshot(source, testSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if backup.SizeBytes != view.SizeBytes() || backup.CreatedAt != view.CreatedUnix() || backup.TransferredBytes != nil || backup.StartedAt != nil {
		t.Fatalf("rebuilt backup = %+v, want manifest size and creation time, unknown runtime facts", backup)
	}
	if summary := sourceSummary(t, lib); len(summary) != 1 || summary[source].RestorePoints != 1 {
		t.Fatalf("summary = %+v, want one restore point of %s", summary, source)
	}

	// Footprint is background maintenance, not part of catalog reconcile.
	if err := lib.Collect(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	// The footprint counts live objects plus the published manifest itself.
	want := backup.SizeBytes + manifestInfo.Size()
	if got := sourceFootprint(t, lib, source); got == nil || *got != want {
		t.Fatalf("scrubbed footprint = %v, want %d", got, want)
	}

	// A restart re-runs reconcile before the async scrub: an unchanged source is
	// still returned for verification and its footprint is not blanked.
	if sources := reconcile(t, lib); len(sources) != 1 || sources[0] != source {
		t.Fatalf("unchanged reconcile sources = %v, want [%s]", sources, source)
	}
	if got := sourceFootprint(t, lib, source); got == nil || *got != want {
		t.Fatalf("reconcile blanked surviving footprint = %v, want %d", got, want)
	}

	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	reconcile(t, lib)
	requireNotCataloged(t, lib, testSnapshot)
	if summary := sourceSummary(t, lib); len(summary) != 0 {
		t.Fatalf("the source outlived its manifest: %+v", summary)
	}
}

// A duplicated device directory publishes one snapshot id twice. Startup keeps
// the first source and skips the copy rather than refusing to run.
func TestReconcileKeepsOneOwnerOfADuplicatedSnapshotID(t *testing.T) {
	lib, root := newTestLibrary(t)
	const kept, ignored = "testphoneudid0041", "testphoneudid0042"
	publishFixture(t, lib, root, kept, testSnapshot)
	copied := publishFixture(t, lib, root, ignored, testSnapshot)

	// Every restart keeps the same owner, never flipping to the copy.
	for pass := range 2 {
		if sources := reconcile(t, lib); len(sources) != 2 {
			t.Fatalf("pass %d: sources = %v, want both", pass, sources)
		}
		row, err := lib.catalog.Backup.Get(context.Background(), testSnapshot)
		if err != nil {
			t.Fatal(err)
		}
		if row.SourceUDID != kept {
			t.Fatalf("pass %d: snapshot owner = %s, want %s", pass, row.SourceUDID, kept)
		}
	}
	// The copy stays on disk, so collection keeps its objects reachable.
	requirePresent(t, copied...)
}

// A source whose directory cannot be read never fails startup: the others
// reconcile, and its own rows stay until it reads again.
func TestStartupSkipsASourceItCannotRead(t *testing.T) {
	lib, root := newTestLibrary(t)
	const broken, healthy = "testphoneudid0051", "testphoneudid0052"
	const healthyID = "dddddddd-0000-4000-8000-000000000004"
	publishFixture(t, lib, root, broken, testSnapshot)
	reconcile(t, lib)
	// A directory under a manifest's name is damage, not a dropping.
	damaged := filepath.Join(root, broken, "snapshots", healthyID+".json")
	if err := os.Mkdir(damaged, 0o755); err != nil {
		t.Fatal(err)
	}
	publishFixture(t, lib, root, healthy, healthyID)

	reconcile(t, lib)
	for _, id := range []string{testSnapshot, healthyID} {
		if _, err := lib.catalog.Backup.Get(context.Background(), id); err != nil {
			t.Fatalf("snapshot %s is not cataloged: %v", id, err)
		}
	}
}

// A corrupt manifest is marked, never removed: a newcomer is admitted damaged,
// and bit rot in an admitted one is found by the scrub, which keeps every
// object the damaged manifest still names.
func TestCorruptManifestIsMarkedAtAdmissionOrByScrub(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0009"
	const newcomerID = "dddddddd-0000-4000-8000-000000000004"
	ctx := context.Background()
	paths := publishFixture(t, lib, root, source, testSnapshot)
	admittedPath := paths[len(paths)-1]
	reconcile(t, lib)
	requireDamage := func(id, want string) {
		t.Helper()
		if row, err := lib.catalog.Backup.Get(ctx, id); err != nil || row.Damage != want {
			t.Fatalf("snapshot %s = %+v, %v; want damage %q", id, row, err, want)
		}
	}

	newcomerPath := filepath.Join(root, source, "snapshots", newcomerID+".json")
	writeTestFile(t, newcomerPath, brokenManifest)
	manifest, err := os.ReadFile(admittedPath)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, admittedPath, bytes.Replace(manifest, []byte("Status.plist"), []byte("Status.plisT"), 1))
	reconcile(t, lib)
	requireDamage(newcomerID, objectstore.DamageManifestUnreadable)
	requireDamage(testSnapshot, "") // an admitted row is trusted until the scrub

	if err := lib.Collect(ctx, source); err != nil {
		t.Fatal(err)
	}
	requireDamage(testSnapshot, objectstore.DamageManifestUnreadable)
	requirePresent(t, append(paths, newcomerPath)...)
}

// A newer format's manifest belongs to a newer AirVault: it is neither admitted
// nor dropped as corrupt, and collection stops rather than sweep what it reaches.
func TestNewerFormatManifestIsKeptAndStopsCollection(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0053"
	const newerID = "dddddddd-0000-4000-8000-000000000004"
	paths := publishFixture(t, lib, root, source, testSnapshot)
	newer := filepath.Join(root, source, "snapshots", newerID+".json")
	writeTestFile(t, newer, []byte(`{"version":2}`))
	orphan := filepath.Join(root, source, "objects", "ff", strings.Repeat("f", 64))
	writeTestFile(t, orphan, []byte("unreferenced"))

	reconcile(t, lib)
	_ = lib.Collect(context.Background(), source)
	lib.Wait()
	requireNotCataloged(t, lib, newerID)
	requirePresent(t, append(paths, newer, orphan)...)
}

func TestCatalogReconcileMovesStaleSnapshotOwnershipToManifestSource(t *testing.T) {
	lib, root := newTestLibrary(t)
	const staleSource = "zsourcephone"
	const manifestSource = "asourcephone"
	stalePaths := publishFixture(t, lib, root, staleSource, testSnapshot)
	if recovered, err := lib.settle(context.Background(), pendingRow(staleSource)); err != nil || !recovered {
		t.Fatalf("recover stale catalog owner: recovered=%v error=%v", recovered, err)
	}
	if err := os.Remove(stalePaths[len(stalePaths)-1]); err != nil {
		t.Fatal(err)
	}
	publishFixture(t, lib, root, manifestSource, testSnapshot)

	if _, err := lib.reconcileCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	backup, err := lib.catalog.Backup.Get(context.Background(), testSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if backup.SourceUDID != manifestSource {
		t.Fatalf("catalog source = %q, want manifest source %q", backup.SourceUDID, manifestSource)
	}
	if backup.StartedAt != nil || backup.TransferredBytes != nil {
		t.Fatalf("runtime facts survived ownership change: %+v", backup)
	}
}

// Recovery collects through the same pass as startup: a bit-rotted sibling is
// kept and admitted as damaged, not left to wedge collection, and the runtime
// facts recovery records survive a later reconcile.
func TestPublishedRecoveryKeepsCorruptSiblingAndRuntimeFacts(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0007"
	const siblingID = "dddddddd-0000-4000-8000-000000000004"
	transferred := int64(1234)
	row := pendingRow(source)
	row.TransferredBytes = &transferred
	publishFixture(t, lib, root, source, testSnapshot)
	corruptPath := filepath.Join(root, source, "snapshots", siblingID+".json")
	writeTestFile(t, corruptPath, brokenManifest)

	recovered, err := lib.settle(context.Background(), row)
	if err != nil || !recovered {
		t.Fatalf("recover published snapshot: recovered=%v error=%v", recovered, err)
	}
	requirePresent(t, corruptPath)
	if got := sourceFootprint(t, lib, source); got == nil {
		t.Fatal("source usage should be refreshed by recovery")
	}

	if _, err := lib.reconcileCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sibling, err := lib.catalog.Backup.Get(context.Background(), siblingID); err != nil || sibling.Damage != objectstore.DamageManifestUnreadable {
		t.Fatalf("sibling = %+v, %v; want it admitted as damaged", sibling, err)
	}
	backup, err := lib.catalog.Backup.Get(context.Background(), testSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if backup.StartedAt == nil || *backup.StartedAt != *row.StartedAt {
		t.Fatalf("start time = %v, want %d", backup.StartedAt, *row.StartedAt)
	}
	if backup.TransferredBytes == nil || *backup.TransferredBytes != transferred {
		t.Fatalf("transferred bytes = %v, want %d", backup.TransferredBytes, transferred)
	}
}

// The run is over whether or not the manifest reads back, so its staging
// envelope must go: a stranded one silently blocks collection for the source.
func TestUnreadableManifestStillClearsStagingEnvelope(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0011"
	stagingDir := writeUnpublishedStaging(t, root, source)
	writeTestFile(t, filepath.Join(root, source, "snapshots", testSnapshot+".json"), brokenManifest)

	recovered, err := lib.settle(context.Background(), pendingRow(source))
	if err == nil || recovered {
		t.Fatalf("unreadable manifest reconciled: recovered=%v error=%v, want an error", recovered, err)
	}
	requireAbsent(t, stagingDir)
}
