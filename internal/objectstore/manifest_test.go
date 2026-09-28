package objectstore

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func entriesChecksum(entries map[string]manifestEntry) string {
	seal := newManifestEntriesSeal()
	for _, key := range slices.Sorted(maps.Keys(entries)) {
		seal.add(key, entries[key])
	}
	return seal.checksum()
}

// Golden vectors of the Rust engine's seal: stores it wrote must stay
// readable. The multi-entry vector pins byte-wise key order ("B" < "a" < UTF-8 "а").
func TestEntriesChecksumMatchesRustVector(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries map[string]manifestEntry
		want    string
	}{
		{
			name: "single",
			entries: map[string]manifestEntry{
				"A<&": {Kind: entryFile, ObjectRef: obj1, Size: 7, ModifiedUnix: 9},
			},
			want: "98bcef975d7c1daa1f331dd9aad8555d15335fcd07c17062ce24380e999aa176",
		},
		{
			name: "key order",
			entries: map[string]manifestEntry{
				"a": {Kind: entryFile, ObjectRef: obj1, Size: 1, ModifiedUnix: 2},
				"B": {Kind: entryDirectory},
				"а": {Kind: entryFile, ObjectRef: obj2, Size: 3, ModifiedUnix: 4},
			},
			want: "42077a9ba0fdb9f9027e23b72e0bb7bfc192a161fc5b8a80127ec3b43cb5a621",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := entriesChecksum(test.entries); got != test.want {
				t.Fatalf("entries checksum = %s, want %s", got, test.want)
			}
		})
	}
}

func TestManifestEntryValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries map[string]manifestEntry
		wantErr string
	}{
		{"invalid content address", map[string]manifestEntry{
			"file": {Kind: entryFile, ObjectRef: "not-a-sha256", Size: 1},
		}, "invalid object reference"},
		{"missing parent", map[string]manifestEntry{
			"missing/file": {Kind: entryFile, ObjectRef: obj1, Size: 1},
		}, "missing parent directory"},
		{"file as parent", map[string]manifestEntry{
			"parent":       {Kind: entryFile, ObjectRef: obj1, Size: 1},
			"parent/child": {Kind: entryFile, ObjectRef: obj2, Size: 1},
		}, "is not a directory"},
		{"excessive depth", map[string]manifestEntry{
			strings.Repeat("a/", maxLogicalDepth) + "a": {Kind: entryDirectory},
		}, "exceeds 128 components"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := manifestProjection{SourceUDID: "testphoneudid0001", SnapshotID: genA, Entries: test.entries}
			if _, err := inspectManifestEntries("manifest.json", &manifest); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validation error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestOpenSnapshotRejectsSealedEntryMutation(t *testing.T) {
	store, source := newTestStore(t)
	manifestPath := filepath.Join(store.root, source, "snapshots", genA+".json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest manifestProjection
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Entries["photo.jpg"] = manifestEntry{Kind: entryFile, ObjectRef: obj4, Size: 50}
	if data, err = json.Marshal(manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenSnapshot(source, genA); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("mutated manifest error = %v, want checksum mismatch", err)
	}
}
