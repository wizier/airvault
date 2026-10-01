package objectstore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wizier/airvault/internal/durable"
)

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

// newTestStore holds a two-snapshot chain: gB inherits an unchanged object
// from gA and adds its own.
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

func writeTestManifest(t *testing.T, root, source, snapshotID string, entries map[string]manifestEntry) {
	t.Helper()
	var sizeBytes int64
	for _, entry := range entries {
		if entry.Kind == entryFile {
			sizeBytes += entry.Size
		}
	}
	m := manifest{
		Version: formatVersion, SourceUDID: source,
		SnapshotID: snapshotID, CreatedUnix: 1, SizeBytes: sizeBytes, Entries: entries,
	}
	m.EntriesSHA256 = entriesChecksum(entries)
	dir := filepath.Join(root, source, "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(m)
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

// No path into a source may leave the store root through a symlink: the
// operation is refused and nothing outside the root is touched.
func TestSourcePathRefusesASubtreeReachedThroughASymlink(t *testing.T) {
	for _, test := range []struct {
		name string
		// link is the component the operation resolves through, relative to root.
		link    func(source string) string
		operate func(store *Store, source string) error
	}{
		{
			name: "manifest listing",
			link: func(source string) string { return filepath.Join(source, "snapshots") },
			operate: func(store *Store, source string) error {
				_, err := store.ListSnapshots(source)
				return err
			},
		},
		{
			name:    "collection",
			link:    func(source string) string { return filepath.Join(source, "objects") },
			operate: func(store *Store, source string) error { return store.Sweep(&Scan{source: source}) },
		},
		{
			name:    "staging recovery",
			link:    func(source string) string { return filepath.Join(source, "staging") },
			operate: func(store *Store, source string) error { return store.RecoverSource(source) },
		},
		{
			name:    "source unpublish",
			link:    func(source string) string { return source },
			operate: func(store *Store, source string) error { return store.UnpublishSource(source) },
		},
		{
			name:    "source tree removal",
			link:    func(source string) string { return source },
			operate: func(store *Store, source string) error { return store.RemoveSourceTree(source) },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, source := newTestStore(t)
			// The link target is outside the root the store owns.
			elsewhere := t.TempDir()
			witness := filepath.Join(elsewhere, "keep-me")
			if err := os.WriteFile(witness, []byte("outside the store"), 0o644); err != nil {
				t.Fatal(err)
			}
			linkPath := filepath.Join(store.root, test.link(source))
			if err := os.RemoveAll(linkPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, linkPath); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			err := test.operate(store, source)
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("operation on a symlinked subtree returned %v, want a symlink refusal", err)
			}
			if _, statErr := os.Stat(witness); statErr != nil {
				t.Fatalf("content outside the store was disturbed: %v", statErr)
			}
		})
	}
}

func stageTestManifest(t *testing.T, store *Store, source, snapshotID string, entries map[string]manifestEntry) (string, string) {
	t.Helper()
	writeTestManifest(t, store.root, source, snapshotID, entries)
	stagingPath := filepath.Join(store.root, source, "staging", snapshotID, "manifest.json")
	if err := os.MkdirAll(filepath.Dir(stagingPath), 0o755); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(store.root, source, "snapshots", snapshotID+".json")
	if err := os.Rename(finalPath, stagingPath); err != nil {
		t.Fatal(err)
	}
	return stagingPath, finalPath
}

func openTestStaging(t *testing.T, store *Store, source, snapshotID string) *StagedSnapshot {
	t.Helper()
	snapshot, err := store.openManifest(source, snapshotID, stagingManifestRelative(source, snapshotID))
	if err != nil {
		t.Fatal(err)
	}
	return &StagedSnapshot{Snapshot{store: snapshot.store, relative: snapshot.relative, manifest: snapshot.manifest}}
}

func TestPublishCommitsStagedManifestOverSharedPool(t *testing.T) {
	store, source := newTestStore(t)
	entries := map[string]manifestEntry{
		"inherited": {Kind: entryFile, ObjectRef: obj1, Size: 100},
		"new":       {Kind: entryFile, ObjectRef: obj3, Size: 40},
	}
	stagingPath, _ := stageTestManifest(t, store, source, genC, entries)
	writeTestObject(t, store.root, source, obj3, 40)
	published, err := store.Publish(openTestStaging(t, store, source, genC))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.OpenSnapshot(source, genC)
	if err != nil {
		t.Fatalf("open published snapshot: %v", err)
	}
	if published.CreatedUnix() != reopened.CreatedUnix() || published.SizeBytes() != reopened.SizeBytes() {
		t.Fatalf("published snapshot diverges from final manifest: (%d,%d) vs (%d,%d)",
			published.CreatedUnix(), published.SizeBytes(), reopened.CreatedUnix(), reopened.SizeBytes())
	}
	if _, err := os.Stat(filepath.Dir(stagingPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging directory survived publication: %v", err)
	}
	// Recovering an already-completed publication is a safe no-op.
	if recovered, err := store.Recover(source, genC); err != nil || recovered == nil {
		t.Fatalf("Recover = %v, %v; want the published snapshot", recovered, err)
	}
}

func TestPublishSyncsContentsBeforeFinalRename(t *testing.T) {
	store, source := newTestStore(t)
	entries := map[string]manifestEntry{"inherited": {Kind: entryFile, ObjectRef: obj1, Size: 100}}
	stagingPath, finalPath := stageTestManifest(t, store, source, genC, entries)

	injected := errors.New("injected content sync failure")
	store.syncContents = func() error { return injected }
	if _, err := store.Publish(openTestStaging(t, store, source, genC)); !errors.Is(err, injected) {
		t.Fatalf("Publish error = %v, want injected sync failure", err)
	}
	if _, err := os.Stat(finalPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("final manifest became visible before content sync: %v", err)
	}
	if _, err := os.Stat(stagingPath); err != nil {
		t.Fatalf("staging manifest was not preserved: %v", err)
	}
}

func TestRecoverCompletesDurabilityAfterFinalRename(t *testing.T) {
	store, source := newTestStore(t)
	entries := map[string]manifestEntry{"inherited": {Kind: entryFile, ObjectRef: obj1, Size: 100}}
	stageTestManifest(t, store, source, genC, entries)

	injected := errors.New("injected directory sync failure")
	snapshotSyncs := 0
	store.syncDir = func(directory string) error {
		if filepath.Base(directory) == "snapshots" {
			snapshotSyncs++
			if snapshotSyncs == 1 {
				return injected
			}
		}
		return durable.SyncDir(directory)
	}
	if _, err := store.Publish(openTestStaging(t, store, source, genC)); !errors.Is(err, injected) {
		t.Fatalf("first Publish error = %v, want injected sync failure", err)
	}
	// The rename crossed before the injected failure: recovery reopens the
	// published manifest and completes the same directory commit.
	if published, err := store.Recover(source, genC); err != nil || published == nil {
		t.Fatalf("Recover = %v, %v; want the snapshot published before the failure", published, err)
	}
	if snapshotSyncs != 2 {
		t.Fatalf("snapshot directory syncs = %d, want retry after rename", snapshotSyncs)
	}
	if _, err := os.Stat(filepath.Join(store.root, source, "staging", genC)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging envelope survived recovery: %v", err)
	}
}

func TestRemoveSnapshotDropsReachabilityRootIdempotently(t *testing.T) {
	store, source := newTestStore(t)
	if err := store.RemoveSnapshot(source, genA); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveSnapshot(source, genA); err != nil {
		t.Fatalf("repeated RemoveSnapshot: %v", err)
	}
	if ids := listedIDs(t, store, source); !slices.Equal(ids, []string{genB}) {
		t.Fatalf("listed snapshots = %v, want [%s]", ids, genB)
	}
}

func TestDiscardLeavesPooledOrphanForMarkAndSweep(t *testing.T) {
	store, source := newTestStore(t)
	writeTestObject(t, store.root, source, obj3, 40)
	pooled, err := store.resolveObjectRef(source, obj3)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(store.root, source, "staging", genC, "objects", "random.tmp")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(pooled, staged); err != nil {
		t.Fatal(err)
	}

	if err := store.Discard(source, genC); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging object survived discard: %v", err)
	}
	if _, err := os.Stat(pooled); err != nil {
		t.Fatalf("pooled orphan disappeared before GC: %v", err)
	}
	if _, err := collectAll(t, store, source); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pooled); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("pooled orphan survived mark-and-sweep: %v", err)
	}
}

// A manifest that no longer decodes is the catalog reconciler's problem. It
// must not keep the store from starting, and its envelope must not survive to
// block collection.
func TestRecoverSourceClearsEnvelopeOfUnreadableManifest(t *testing.T) {
	store, source := newTestStore(t)
	stagingDir := filepath.Join(store.root, source, "staging", genA)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(store.root, source, "snapshots", genA+".json")
	if err := os.WriteFile(manifestPath, []byte(`{"broken":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.RecoverSource(source); err != nil {
		t.Fatalf("unreadable manifest blocked staging recovery: %v", err)
	}
	if _, err := os.Stat(stagingDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging envelope survived: %v", err)
	}
	if err := store.ensureCollectable(source); err != nil {
		t.Fatalf("staging still blocks collection: %v", err)
	}
}

// Finder, SMB and NAS droppings are not the store's: every listing skips them,
// they never block collection and are never removed.
func TestForeignEntriesAreSkippedAndKept(t *testing.T) {
	store, source := newTestStore(t)
	sourceRoot := filepath.Join(store.root, source)
	dirs := []string{
		filepath.Join(sourceRoot, "staging", "@eaDir"),
		filepath.Join(sourceRoot, "snapshots", "@eaDir"),
		filepath.Join(sourceRoot, "objects", "@eaDir"),
	}
	files := []string{
		filepath.Join(sourceRoot, "snapshots", ".DS_Store"),
		filepath.Join(sourceRoot, "objects", "Thumbs.db"),
		filepath.Join(sourceRoot, "objects", obj1[:objectPrefixLength], "desktop.ini"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range files {
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.RecoverSource(source); err != nil {
		t.Fatalf("staging recovery: %v", err)
	}
	if ids := listedIDs(t, store, source); !slices.Equal(ids, []string{genA, genB}) {
		t.Fatalf("listed snapshots = %v; want [%s %s]", ids, genA, genB)
	}
	if _, err := collectAll(t, store, source); err != nil {
		t.Fatalf("collection: %v", err)
	}
	for _, path := range append(dirs, files...) {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("foreign entry %s was removed: %v", path, err)
		}
	}
}

func listedIDs(t *testing.T, store *Store, source string) []string {
	t.Helper()
	snapshots, err := store.ListSnapshots(source)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, snapshot := range snapshots {
		ids = append(ids, snapshot.ID)
	}
	return ids
}
