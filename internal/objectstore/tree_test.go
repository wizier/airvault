package objectstore

import (
	"maps"
	"reflect"
	"slices"
	"testing"
)

func addFile(t *testing.T, tr *tree, key string) {
	t.Helper()
	if err := tr.insertFile(key, obj1, 1, 7); err != nil {
		t.Fatal(err)
	}
}

func keys(tr *tree) []string { return slices.Sorted(maps.Keys(tr.entries())) }

// Subtree operations follow the structure, not string order: "Snapshot" and
// "Snapshot.plist" are siblings, not parent and child.
func TestTreeSubtreeOperationsIgnoreLexicographicSiblings(t *testing.T) {
	tr := newTree()
	addFile(t, tr, "Snapshot/Manifest.db")
	addFile(t, tr, "Snapshot.plist")
	if err := tr.rename("Snapshot", "Moved", 8); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Moved", "Moved/Manifest.db", "Snapshot.plist"}; !slices.Equal(keys(tr), want) {
		t.Fatalf("after rename: %v, want %v", keys(tr), want)
	}
	tr.remove("Moved")
	if want := []string{"Snapshot.plist"}; !slices.Equal(keys(tr), want) {
		t.Fatalf("after remove: %v, want %v", keys(tr), want)
	}
}

func TestTreeMoveNeverClobbersAPopulatedTarget(t *testing.T) {
	tr := newTree()
	addFile(t, tr, "a/file")
	addFile(t, tr, "b/kept")
	if err := tr.rename("a", "b", 8); err == nil {
		t.Fatal("moving a directory over a populated one succeeded")
	}
	if err := tr.rename("a/file", "a", 8); err == nil {
		t.Fatal("moving a directory into itself succeeded")
	}
	if err := tr.rename("a", "a", 8); err != nil {
		t.Fatalf("a self-rename must be a no-op: %v", err)
	}
	if !slices.Contains(keys(tr), "b/kept") || !slices.Contains(keys(tr), "a/file") {
		t.Fatalf("a refused move changed the tree: %v", keys(tr))
	}
	if err := tr.insertFile("a/file/child", obj1, 1, 8); err == nil {
		t.Fatal("a file became a directory")
	}
	if err := tr.insertFile("a", obj1, 1, 8); err == nil {
		t.Fatal("a directory became a file")
	}
}

func TestTreeMergeClonesAndSkipsConflicts(t *testing.T) {
	tr := newTree()
	addFile(t, tr, "src/one")
	addFile(t, tr, "src/sub/two")
	addFile(t, tr, "src/clash")
	addFile(t, tr, "dst/keep")
	if _, err := tr.ensureDir("dst/clash", 7); err != nil {
		t.Fatal(err)
	}
	tr.merge("dst", tr.get("src"), 9)
	want := []string{"dst", "dst/clash", "dst/keep", "dst/one", "dst/sub", "dst/sub/two", "src", "src/clash", "src/one", "src/sub", "src/sub/two"}
	if got := keys(tr); !slices.Equal(got, want) {
		t.Fatalf("after merge: %v\nwant %v", got, want)
	}
	// The copy is independent of its source.
	tr.remove("src/sub")
	if tr.get("dst/sub/two") == nil {
		t.Fatal("the merged copy shares nodes with its source")
	}
	if tr.get("dst/one").modifiedUnix != 9 {
		t.Fatal("copied files take the copy time")
	}
}

func TestTreeRoundTripsThroughEntries(t *testing.T) {
	tr := newTree()
	addFile(t, tr, "a/b/c")
	if _, err := tr.ensureDir("empty", 3); err != nil {
		t.Fatal(err)
	}
	entries := tr.entries()
	if again := treeFromEntries(entries).entries(); !reflect.DeepEqual(again, entries) {
		t.Fatalf("round trip changed entries:\n%v\n%v", entries, again)
	}
}
