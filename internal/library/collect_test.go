package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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

// recordSnapshot chooses between seeding the size, shifting it and leaving it
// unknown. Seeding one that was merely unknown would report less than what is
// really on disk, and nothing downstream would notice.
func TestRecordingSeedsThenShiftsButNeverInventsAFootprint(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0021"
	first, second := int64(1000), int64(250)
	publish := func(snapshotID string, added int64) {
		t.Helper()
		publishFixture(t, lib, root, source, snapshotID)
		view, err := lib.objects.OpenSnapshot(source, snapshotID)
		if err != nil {
			t.Fatal(err)
		}
		projection, err := Project(view)
		if err != nil {
			t.Fatal(err)
		}
		if err := lib.recordSnapshot(context.Background(), projection, &added); err != nil {
			t.Fatal(err)
		}
	}

	publish("dddddddd-0000-4000-8000-00000000000a", first)
	if got := sourceFootprint(t, lib, source); got == nil || *got != first {
		t.Fatalf("seeded footprint = %v, want %d", got, first)
	}

	publish("dddddddd-0000-4000-8000-00000000000b", second)
	if got := sourceFootprint(t, lib, source); got == nil || *got != first+second {
		t.Fatalf("shifted footprint = %v, want %d", got, first+second)
	}

	if err := lib.catalog.Backup.ForgetSourceFootprint(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	publish("dddddddd-0000-4000-8000-00000000000c", second)
	if got := sourceFootprint(t, lib, source); got != nil {
		t.Fatalf("footprint on an unknown base = %d, want unknown", *got)
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

func TestReclaimStopsWhenCorruptManifestCannotBeRemoved(t *testing.T) {
	lib, root := newTestLibrary(t)
	const source = "testphoneudid0019"
	paths := publishFixture(t, lib, root, source, testSnapshot)
	reconcile(t, lib)
	manifestPath := paths[len(paths)-1]
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	// A directory is provably not a valid manifest, but RemoveSnapshot refuses
	// to unlink it. The incomplete live set must never reach CollectLive.
	if err := os.Mkdir(manifestPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := lib.Collect(context.Background(), source); err == nil {
		t.Fatal("reclaim succeeded without removing the corrupt manifest")
	}
	requirePresent(t, paths[:len(paths)-1]...)
}
