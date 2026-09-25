package objectstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRemoveSnapshotDropsReachabilityRootIdempotently(t *testing.T) {
	store, source := newTestStore(t)
	if err := store.RemoveSnapshot(source, genA); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveSnapshot(source, genA); err != nil {
		t.Fatalf("repeated RemoveSnapshot: %v", err)
	}
	ids, err := store.ListSnapshotIDs(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != genB {
		t.Fatalf("ListSnapshotIDs = %v, want [%s]", ids, genB)
	}
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
				_, err := store.ListSnapshotIDs(source)
				return err
			},
		},
		{
			name:    "collection",
			link:    func(source string) string { return filepath.Join(source, "objects") },
			operate: func(store *Store, source string) error { return store.CollectLive(source, &LiveSet{}) },
		},
		{
			name:    "staging reconcile",
			link:    func(source string) string { return filepath.Join(source, "staging") },
			operate: func(store *Store, source string) error { return store.ReconcileSourceStaging(source) },
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
