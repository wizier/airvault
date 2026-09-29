package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/objectstore"
)

func TestDeleteSnapshotsRecountsUsageThenEmptiesTheSource(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0014"
	const keptID = "eeeeeeee-0000-4000-8000-000000000005"
	publishFixture(t, lib, root, source, testSnapshot)
	keptPaths := publishFixture(t, lib, root, source, keptID)
	reconcile(t, lib)
	released := 0
	release := func() { released++ }

	if err := lib.DeleteSnapshots(context.Background(), source, []string{testSnapshot}, release); err != nil {
		t.Fatal(err)
	}
	requireNotCataloged(t, lib, testSnapshot)
	kept, err := lib.catalog.Backup.Get(context.Background(), keptID)
	if err != nil {
		t.Fatal(err)
	}
	manifestInfo, err := os.Stat(keptPaths[len(keptPaths)-1])
	if err != nil {
		t.Fatal(err)
	}
	want := kept.SizeBytes + manifestInfo.Size()
	// The size is published within the request, before the sweep runs behind it.
	if got := sourceFootprint(t, lib, source); got == nil || *got != want {
		t.Fatalf("footprint after deletion = %v, want %d", got, want)
	}
	// The sweep works from the set the recount measured, so that size is final.
	lib.Wait()
	if got := sourceFootprint(t, lib, source); got == nil || *got != want {
		t.Fatalf("footprint after collection = %v, want %d", got, want)
	}

	// With the last snapshot gone nothing is reachable: collection takes manifest,
	// objects and directories alike, so the source stops being listed.
	if err := lib.DeleteSnapshots(context.Background(), source, []string{keptID}, release); err != nil {
		t.Fatal(err)
	}
	lib.Wait()
	requireAbsent(t, filepath.Join(root, source))
	if sources, err := lib.objects.ListSources(); err != nil || len(sources) != 0 {
		t.Fatalf("emptied source is still listed: %v, %v", sources, err)
	}
	if released != 2 {
		t.Fatalf("the write lease was released %d times, want once per deletion", released)
	}
}

// Deleting a device never reads a manifest and never asks the collector what it
// recognises: whatever the source left behind, the tree goes.
func TestDeleteSourceRemovesWhateverItLeftBehind(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0002"
	writeUnpublishedStaging(t, root, source)
	writeTestFile(t, filepath.Join(root, source, "objects", "not-an-object-prefix"), []byte("junk"))
	writeTestFile(t, filepath.Join(root, source, "snapshots", testSnapshot+".json"), brokenManifest)

	released := false
	if err := lib.DeleteSource(context.Background(), source, func() { released = true }); err != nil {
		t.Fatal(err)
	}
	// Reclamation runs in the background and owns the write lease.
	lib.Wait()
	requireAbsent(t, filepath.Join(root, source))
	if !released {
		t.Fatal("the write lease was not released after reclamation")
	}
}

// Reconcile drops an abandoned envelope but leaves what it pooled: the ids in
// the catalog and on disk then agree, and only collection finds the orphan.
func TestCollectReclaimsWhatAnAbandonedSnapshotPooled(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0017"
	const abandonedID = "eeeeeeee-0000-4000-8000-000000000005"
	publishFixture(t, lib, root, source, testSnapshot)
	reconcile(t, lib)
	const objectRef = "9999999999999999999999999999999999999999999999999999999999999999"
	staged := filepath.Join(root, source, "staging", abandonedID, "objects", "orphan.tmp")
	writeTestFile(t, staged, []byte("abandoned object"))
	pooled := filepath.Join(root, source, "objects", objectRef[:2], objectRef)
	if err := os.MkdirAll(filepath.Dir(pooled), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(staged, pooled); err != nil {
		t.Fatal(err)
	}
	if sources := reconcile(t, lib); len(sources) != 1 {
		t.Fatalf("sources = %v, want the source back for collection", sources)
	}
	requirePresent(t, pooled)
	if err := lib.Collect(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, pooled)
}

// A manifest name that is not a file cannot be read or judged, so collection
// stops instead of sweeping what that snapshot might reach.
func TestCollectStopsAtAManifestThatIsNotAFile(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0019"
	paths := publishFixture(t, lib, root, source, testSnapshot)
	reconcile(t, lib)
	manifestPath := paths[len(paths)-1]
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(manifestPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := lib.Collect(context.Background(), source); err == nil {
		t.Fatal("collection went past a manifest it could not read")
	}
	requirePresent(t, paths[:len(paths)-1]...)
}

// A restore point that lost an object is marked damaged, never removed: it
// can't be opened or serve as a base, and is whole again once the object is
// back. With every one damaged, the next backup has no base: a full one.
func TestCollectMarksRestorePointsMissingObjects(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0022"
	const newerID = "dddddddd-0000-4000-8000-000000000004"
	ctx := context.Background()
	older := maps.Clone(fixtureFiles)
	older["notes.txt"] = "in both restore points"
	newer := maps.Clone(older)
	newer["photo.jpg"] = "only in the newer restore point"
	publishFiles(t, lib, root, source, testSnapshot, older)
	newerPaths := publishFiles(t, lib, root, source, newerID, newer)
	reconcile(t, lib)
	collect := func() {
		t.Helper()
		if err := lib.Collect(ctx, source); err != nil {
			t.Fatal(err)
		}
	}
	requireBase := func(want string) {
		t.Helper()
		base, err := lib.LatestBase(ctx, source)
		if err != nil || (base == nil) != (want == "") || (base != nil && base.ID() != want) {
			t.Fatalf("LatestBase = %v, %v; want %q", base, err, want)
		}
	}

	removeObject(t, root, source, newer["photo.jpg"])
	collect()
	if row, err := lib.catalog.Backup.Get(ctx, newerID); err != nil || row.Damage != objectstore.DamageFilesMissing || row.DamagedFiles != 1 {
		t.Fatalf("newer row = %+v, %v; want one file missing", row, err)
	}
	requirePresent(t, newerPaths[len(newerPaths)-1])
	// Finding the same damage again changes nothing, so nothing is logged again.
	scan, err := lib.objects.Scan(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := lib.recordHealth(ctx, source, scan); err != nil || len(changed) != 0 {
		t.Fatalf("recorded again = %+v, %v; want no change", changed, err)
	}
	_, err = lib.Open(ctx, newerID)
	if validation, ok := errors.AsType[*domain.ValidationError](err); !ok || validation.Code != "backup_damaged" {
		t.Fatalf("Open damaged = %v, want backup_damaged", err)
	}
	requireBase(testSnapshot)

	writeTestFile(t, objectPath(root, source, newer["photo.jpg"]), []byte(newer["photo.jpg"]))
	collect()
	requireBase(newerID)

	if err := lib.Verify(ctx, source, func(int64, int64) {}); err != nil {
		t.Fatal(err)
	}
	if row, err := lib.catalog.Backup.Get(ctx, newerID); err != nil || row.Damage != "" || row.VerifiedAt == nil {
		t.Fatalf("verified row = %+v, %v; want whole and verified", row, err)
	}

	removeObject(t, root, source, older["notes.txt"])
	collect()
	requireBase("")
	requirePresent(t, objectPath(root, source, older["Manifest.db"]))
}

func objectPath(root, source, content string) string {
	sum := sha256.Sum256([]byte(content))
	objectRef := hex.EncodeToString(sum[:])
	return filepath.Join(root, source, "objects", objectRef[:2], objectRef)
}

func removeObject(t *testing.T, root, source, content string) {
	t.Helper()
	if err := os.Remove(objectPath(root, source, content)); err != nil {
		t.Fatal(err)
	}
}
