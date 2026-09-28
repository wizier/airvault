package objectstore

import (
	"encoding/json"
	"io"
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

// Golden vectors pin the seal: existing stores must stay readable. The
// multi-entry vector pins byte-wise key order ("B" < "a" < UTF-8 "а").
func TestEntriesChecksumMatchesGoldenVector(t *testing.T) {
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

// A format 1 snapshot as an earlier release wrote it: the layout, the field
// names and the seal are what existing stores hold.
func TestOpenSnapshotReadsFormatOne(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	const source = "testphoneudid0001"
	const ref = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	files := map[string]string{
		"snapshots/" + genA + ".json": `{"version":1,"sourceUdid":"` + source + `","snapshotId":"` + genA + `",` +
			`"createdUnix":1700000000,"sizeBytes":5,` +
			`"entriesSha256":"7ed12164b7b06c61d83459be84998d051a75e61868bc585618713da7ffa3221d","entries":{` +
			`"Documents":{"kind":"directory","modifiedUnix":5},` +
			`"Documents/note.txt":{"kind":"file","objectRef":"` + ref + `","size":5,"modifiedUnix":7}}}`,
		"objects/2c/" + ref: "hello",
	}
	for name, content := range files {
		path := filepath.Join(root, source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	view, err := store.OpenSnapshot(source, genA)
	if err != nil {
		t.Fatal(err)
	}
	if view.CreatedUnix() != 1_700_000_000 || view.SizeBytes() != 5 {
		t.Fatalf("created %d, size %d", view.CreatedUnix(), view.SizeBytes())
	}
	file, err := view.Open("Documents/note.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if data, err := io.ReadAll(file); err != nil || string(data) != "hello" {
		t.Fatalf("content = %q, %v", data, err)
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
			strings.Repeat("a/", maxKeyDepth) + "a": {Kind: entryDirectory},
		}, "invalid key"},
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

func TestValidKey(t *testing.T) {
	long := strings.Repeat("a", 255)
	for key, want := range map[string]bool{
		"Manifest.db": true, "ab/abc": true, long: true, long + "a": false,
		strings.Repeat("a/", 127) + "a": true, strings.Repeat("a/", 128) + "a": false,
		strings.Repeat(strings.Repeat("a", 200)+"/", 20) + strings.Repeat("a", 76): true,
		strings.Repeat(strings.Repeat("a", 200)+"/", 20) + strings.Repeat("a", 77): false,
		"": false, ".": false, "/abs": false, "a//b": false, "a/../b": false, "a/./b": false, "a/": false,
		"a\\b": false, "a\x00b": false, "a b": false, "a b": false, "a/\xff": false,
	} {
		if got := ValidKey(key); got != want {
			t.Errorf("ValidKey(%.40q) = %v, want %v", key, got, want)
		}
	}
}
