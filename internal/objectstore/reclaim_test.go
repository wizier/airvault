package objectstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Two-snapshot chain: gB inherits an unchanged object from gA and adds its own.
const (
	genA = "aaaaaaaa-0000-4000-8000-000000000001"
	genB = "bbbbbbbb-0000-4000-8000-000000000002"
	genC = "cccccccc-0000-4000-8000-000000000003"
	obj1 = "1111111111111111111111111111111111111111111111111111111111111111"
	obj2 = "2222222222222222222222222222222222222222222222222222222222222222"
	obj3 = "3333333333333333333333333333333333333333333333333333333333333333"
	obj4 = "4444444444444444444444444444444444444444444444444444444444444444"
	obj5 = "5555555555555555555555555555555555555555555555555555555555555555"
)

func writeTestManifest(t *testing.T, root, source, snapshotID string, entries map[string]manifestEntry) {
	t.Helper()
	var sizeBytes int64
	for _, entry := range entries {
		if entry.Kind == entryFile {
			sizeBytes += entry.Size
		}
	}
	manifest := manifestProjection{
		Version: formatVersion, SourceUDID: source,
		SnapshotID: snapshotID, CreatedUnix: 1, SizeBytes: sizeBytes, Entries: entries,
	}
	manifest.EntriesSHA256 = entriesChecksum(entries)
	dir := filepath.Join(root, source, "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestObject(t *testing.T, root, source, objectRef string, size int64) {
	t.Helper()
	objectPath := filepath.Join(root, source, "objects", objectRef[:objectPrefixLength], objectRef)
	if err := os.MkdirAll(filepath.Dir(objectPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectPath, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

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

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	const source = "testphoneudid0001"
	// gA (older): two unique objects.
	writeTestManifest(t, root, source, genA, map[string]manifestEntry{
		"Manifest.db": {Kind: entryFile, ObjectRef: obj1, Size: 100},
		"photo.jpg":   {Kind: entryFile, ObjectRef: obj2, Size: 50},
		"copy.jpg":    {Kind: entryFile, ObjectRef: obj2, Size: 50}, // shares obj2
		"DCIM":        {Kind: entryDirectory},
	})
	// gB (newer): keeps obj1 (unchanged, inherited from gA), adds obj4 + obj5.
	writeTestManifest(t, root, source, genB, map[string]manifestEntry{
		"Manifest.db": {Kind: entryFile, ObjectRef: obj1, Size: 100}, // inherited
		"photo.jpg":   {Kind: entryFile, ObjectRef: obj4, Size: 70},
		"new.jpg":     {Kind: entryFile, ObjectRef: obj5, Size: 30},
	})
	writeTestObject(t, root, source, obj1, 100)
	writeTestObject(t, root, source, obj2, 50)
	writeTestObject(t, root, source, obj4, 70)
	writeTestObject(t, root, source, obj5, 30)
	return store, source
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
