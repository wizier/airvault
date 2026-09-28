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
	live, err := store.LiveObjects(context.Background(), source, nil)
	if err != nil {
		return 0, err
	}
	if err := store.CollectLive(source, live); err != nil {
		return 0, err
	}
	return live.Footprint(), nil
}

func manifestFileBytes(t *testing.T, store *Store, source string, snapshotIDs ...string) int64 {
	t.Helper()
	var total int64
	for _, snapshotID := range snapshotIDs {
		size, err := store.SnapshotManifestBytes(source, snapshotID)
		if err != nil {
			t.Fatal(err)
		}
		total += size
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

// Pool damage — a truncated or missing live object — is reported, never a
// wedge: the sweep still reclaims garbage and keeps damaged objects on disk
// for partial recovery.
func TestCollectSurvivesDamagedLiveObjects(t *testing.T) {
	store, source := newTestStore(t)
	truncated, err := store.resolveObjectRef(source, obj4)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(truncated, 1); err != nil {
		t.Fatal(err)
	}
	missing, err := store.resolveObjectRef(source, obj5)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	orphanRef := "9999999999999999999999999999999999999999999999999999999999999999"
	writeTestObject(t, store.root, source, orphanRef, 7)
	orphan, _ := store.resolveObjectRef(source, orphanRef)

	if _, err := collectAll(t, store, source); err != nil {
		t.Fatalf("Collect wedged on pool damage: %v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("garbage survived a sweep with pool damage: %v", err)
	}
	if _, err := os.Stat(truncated); err != nil {
		t.Fatalf("damaged live object was deleted: %v", err)
	}
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

func TestReclaimableBytesHonorsCancellation(t *testing.T) {
	store, source := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReclaimableBytes(ctx, source, []string{genA}); err == nil {
		t.Fatal("ReclaimableBytes ignored a cancelled context")
	}
}
