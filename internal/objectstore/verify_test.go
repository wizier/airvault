package objectstore

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// What a check cannot read is not whole: it is set aside like what fails its
// hash, the rest is still checked, and the check reports the failed read.
func TestVerifySetsAsideWhatItCannotRead(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	draft, err := store.BeginSnapshot(draftSource, snapFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	contents := []string{"first", "second", "third"}
	for _, content := range contents {
		writeKey(t, draft, content, content)
	}
	publish(t, store, draft)
	// In the order the check walks them, so the failed read comes first.
	slices.SortFunc(contents, func(a, b string) int { return strings.Compare(refOf(a), refOf(b)) })
	pathOf := func(content string) string {
		path, err := store.resolveObjectRef(draftSource, refOf(content))
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	unreadable, whole, corrupt := pathOf(contents[0]), pathOf(contents[1]), pathOf(contents[2])
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte(strings.Repeat("!", len(contents[2]))), 0o644); err != nil {
		t.Fatal(err)
	}

	verify := func(wantSetAside int, wantFailed bool, wantDamagedFiles int) {
		t.Helper()
		ctx := context.Background()
		scan, err := store.Scan(ctx, draftSource)
		if err != nil {
			t.Fatal(err)
		}
		setAside, err := store.Verify(ctx, scan, func(int64, int64) {})
		if setAside != wantSetAside || (err != nil) != wantFailed {
			t.Fatalf("Verify = %d, %v; want %d set aside, failed: %v", setAside, err, wantSetAside, wantFailed)
		}
		if scan, err = store.Scan(ctx, draftSource); err != nil || scan.Snapshots[0].DamagedFiles != wantDamagedFiles {
			t.Fatalf("health = %+v, %v; want %d damaged files", scan.Snapshots, err, wantDamagedFiles)
		}
	}
	verify(2, true, 2)
	requirePath(t, unreadable+damagedSuffix, true)
	requirePath(t, corrupt+damagedSuffix, true)
	requirePath(t, whole, true)
	// A copy that still does not read stays aside and fails no later check.
	verify(0, false, 2)
	if err := os.Chmod(unreadable+damagedSuffix, 0o644); err != nil {
		t.Fatal(err)
	}
	verify(0, false, 1)
	requirePath(t, unreadable, true)
}
