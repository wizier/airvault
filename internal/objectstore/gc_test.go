package objectstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// collectAll sweeps a source and reports the size of what survived.
func collectAll(t *testing.T, store *Store, source string) (int64, error) {
	t.Helper()
	scan, err := store.Scan(context.Background(), source)
	if err != nil {
		return 0, err
	}
	if err := store.Sweep(scan); err != nil {
		return 0, err
	}
	return scan.Footprint(), nil
}

func requireObjects(t *testing.T, store *Store, source string, present bool, objectRefs ...string) {
	t.Helper()
	for _, objectRef := range objectRefs {
		path, _ := store.resolveObjectRef(source, objectRef)
		requirePath(t, path, present)
	}
}

func requirePath(t *testing.T, path string, present bool) {
	t.Helper()
	if _, err := os.Stat(path); (err == nil) != present {
		t.Fatalf("%s: present = %v, want %v", path, err == nil, present)
	}
}

func manifestFileBytes(t *testing.T, store *Store, source string, snapshotIDs ...string) int64 {
	t.Helper()
	snapshots, err := store.ListSnapshots(source)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, snapshot := range snapshots {
		if slices.Contains(snapshotIDs, snapshot.ID) {
			total += snapshot.size
		}
	}
	return total
}

func TestCollectAfterMiddleSnapshotDeletionKeepsSharedObjects(t *testing.T) {
	store, source := newTestStore(t)
	if err := store.RemoveSnapshot(source, genA); err != nil {
		t.Fatal(err)
	}
	bytes, err := collectAll(t, store, source)
	if err != nil {
		t.Fatal(err)
	}
	manifests := manifestFileBytes(t, store, source, genB)
	// gB's unique objects (100 + 70 + 30) plus its surviving manifest.
	if want := 200 + manifests; bytes != want {
		t.Fatalf("footprint = %d, want %d", bytes, want)
	}
	removed, _ := store.resolveObjectRef(source, obj2)
	if _, err := os.Stat(removed); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unreferenced object survived: %v", err)
	}
	for _, objectRef := range []string{obj1, obj4, obj5} {
		kept, _ := store.resolveObjectRef(source, objectRef)
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("live object %s was removed: %v", objectRef, err)
		}
	}
}

// A source that reaches nothing keeps no objects and no directories, so it
// stops being listed.
func TestCollectAllowsEmptyLiveSet(t *testing.T) {
	store, source := newTestStore(t)
	for _, id := range []string{genA, genB} {
		if err := store.RemoveSnapshot(source, id); err != nil {
			t.Fatal(err)
		}
	}
	if sources, err := store.ListSources(); err != nil || !slices.Equal(sources, []string{source}) {
		t.Fatalf("ListSources before collection = %v, %v; want [%s]", sources, err, source)
	}
	if bytes, err := collectAll(t, store, source); err != nil || bytes != 0 {
		t.Fatalf("Collect with no manifests = %d, %v; want 0, nil", bytes, err)
	}
	if _, err := os.Lstat(filepath.Join(store.root, source)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("source tree survived empty live set: %v", err)
	}
	if sources, err := store.ListSources(); err != nil || len(sources) != 0 {
		t.Fatalf("ListSources after collection = %v, %v; want none", sources, err)
	}
}

// Pool damage never wedges the sweep or costs a snapshot its objects. A lost
// object marks its snapshot damaged, as does a truncated one once a read sets
// it aside, and whole copies coming back make the snapshot whole again.
func TestCollectSurvivesDamagedLiveObjects(t *testing.T) {
	store, source := newTestStore(t)
	truncated, err := store.resolveObjectRef(source, obj4)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(truncated, 1); err != nil {
		t.Fatal(err)
	}
	lost, err := store.resolveObjectRef(source, obj5)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(lost, lost+damagedSuffix); err != nil { // as Verify sets it aside
		t.Fatal(err)
	}
	orphanRef := "9999999999999999999999999999999999999999999999999999999999999999"
	writeTestObject(t, store.root, source, orphanRef, 7)

	scan, err := store.Scan(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	want := []SnapshotHealth{{ID: genA}, {ID: genB, Damage: DamageFilesMissing, DamagedFiles: 1}}
	if !slices.Equal(scan.Snapshots, want) {
		t.Fatalf("health = %+v, want %+v", scan.Snapshots, want)
	}
	if err := store.Sweep(scan); err != nil {
		t.Fatalf("sweep wedged on pool damage: %v", err)
	}
	requireObjects(t, store, source, false, orphanRef)
	requireObjects(t, store, source, true, obj4)
	requirePath(t, lost+damagedSuffix, true)

	viewB, err := store.OpenSnapshot(source, genB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := viewB.Open("photo.jpg"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("open truncated object = %v, want ErrIntegrity", err)
	}
	if scan, err = store.Scan(context.Background(), source); err != nil || scan.Snapshots[1].DamagedFiles != 2 {
		t.Fatalf("health with the truncated object read = %+v, %v; want 2 damaged files", scan.Snapshots, err)
	}

	writeTestObject(t, store.root, source, obj4, 70)
	writeTestObject(t, store.root, source, obj5, 30)
	if scan, err = store.Scan(context.Background(), source); err != nil || scan.Snapshots[1].Damage != "" {
		t.Fatalf("health with the objects back = %+v, %v; want whole", scan.Snapshots, err)
	}
	if err := store.Sweep(scan); err != nil {
		t.Fatal(err)
	}
	requirePath(t, lost+damagedSuffix, false)
	requirePath(t, truncated+damagedSuffix, false)
}

func TestCollectRefusesMutableStaging(t *testing.T) {
	store, source := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(store.root, source, "staging", genC), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := collectAll(t, store, source); err == nil {
		t.Fatal("Collect succeeded while a staging snapshot existed")
	}
}

// copy.jpg shares obj2 with photo.jpg: a snapshot's size counts both entries,
// while the footprint counts the pooled content once, plus the manifests.
func TestSizeCountsEntriesButFootprintCountsPooledContent(t *testing.T) {
	store, source := newTestStore(t)
	viewA, err := store.OpenSnapshot(source, genA)
	if err != nil {
		t.Fatal(err)
	}
	if got := viewA.SizeBytes(); got != 200 {
		t.Fatalf("SizeBytes(gA) = %d, want 200", got)
	}
	manifests := manifestFileBytes(t, store, source, genA, genB)
	got, err := collectAll(t, store, source)
	if err != nil {
		t.Fatal(err)
	}
	// Unique live payload (obj1+obj2+obj4+obj5 = 250) plus both manifests.
	if want := 250 + manifests; got != want {
		t.Fatalf("Collect footprint = %d, want %d", got, want)
	}
}

func TestReclaimableExcludesSharedObjects(t *testing.T) {
	store, source := newTestStore(t)
	reclaimable := func(ids ...string) int64 {
		t.Helper()
		got, err := store.ReclaimableBytes(context.Background(), source, ids)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// gB still references obj1, so deleting gA frees only obj2; deleting gB frees
	// its new objects; deleting both frees the shared obj1 too, more than the two
	// single-deletion answers add up to.
	if a, b, both := reclaimable(genA), reclaimable(genB), reclaimable(genA, genB); a != 50 || b != 100 || both != 250 {
		t.Fatalf("reclaimable gA=%d gB=%d both=%d, want 50, 100, 250", a, b, both)
	}
	if err := store.RemoveSnapshot(source, genB); err != nil {
		t.Fatal(err)
	}
	// With no other published survivor, everything gA references is reclaimable.
	if got := reclaimable(genA); got != 150 {
		t.Fatalf("reclaimable gA alone = %d, want 150", got)
	}
}

// The estimate keeps what Sweep keeps: an unreadable sibling holds every
// object it mentions, and an unreadable target only makes the answer a floor.
func TestReclaimableAgreesWithSweepOnUnreadableManifests(t *testing.T) {
	store, source := newTestStore(t)
	broken := []byte(`{"broken":true,"names":"` + obj2 + `"}`)
	if err := os.WriteFile(filepath.Join(store.root, source, "snapshots", genC+".json"), broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReclaimableBytes(context.Background(), source, []string{genA}); err != nil || got != 0 {
		t.Fatalf("reclaimable gA = %d, %v; want 0: the unreadable sibling keeps obj2", got, err)
	}
	if got, err := store.ReclaimableBytes(context.Background(), source, []string{genC}); err != nil || got != 0 {
		t.Fatalf("reclaimable of the unreadable one = %d, %v; want 0", got, err)
	}
	if got, err := store.ReclaimableBytes(context.Background(), source, []string{genA, genC}); err != nil || got != 50 {
		t.Fatalf("reclaimable gA with the unreadable one = %d, %v; want 50", got, err)
	}
}

func TestReclaimableBytesHonorsCancellation(t *testing.T) {
	store, source := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReclaimableBytes(ctx, source, []string{genA}); err == nil {
		t.Fatal("ReclaimableBytes ignored a cancelled context")
	}
}
