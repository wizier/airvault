package objectstore

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// entriesChecksum seals a finished entry map. Loading a manifest seals as it
// validates; this builds fixtures and the golden vectors.
func entriesChecksum(manifest *manifestProjection) (string, error) {
	if manifest == nil {
		return "", errors.New("manifest is nil")
	}
	seal := newManifestEntriesSeal()
	for _, key := range slices.Sorted(maps.Keys(manifest.Entries)) {
		seal.add(key, manifest.Entries[key])
	}
	return seal.checksum(), nil
}

// Golden vectors shared with Rust (manifest_seal_matches_go_golden): both must
// hash identically or the seal is unportable. The multi-entry vector pins
// byte-wise key order ("B" < "a" < UTF-8 "а").
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
			got, err := entriesChecksum(&manifestProjection{Entries: test.entries})
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("entries checksum = %s, want %s", got, test.want)
			}
		})
	}
}

func TestManifestRejectsInvalidContentAddress(t *testing.T) {
	manifest := manifestProjection{
		Version: formatVersion, SourceUDID: "testphoneudid0001",
		SnapshotID: genA, CreatedUnix: 1, SizeBytes: 1,
		Entries: map[string]manifestEntry{
			"file": {Kind: entryFile, ObjectRef: "not-a-sha256", Size: 1},
		},
	}
	if _, err := inspectManifestEntries("manifest.json", &manifest); err == nil || !strings.Contains(err.Error(), "invalid object reference") {
		t.Fatalf("validation error = %v, want invalid object reference", err)
	}
}

func TestManifestRequiresExplicitDirectoryParents(t *testing.T) {
	manifest := manifestProjection{
		SourceUDID: "testphoneudid0001", SizeBytes: 1,
		Entries: map[string]manifestEntry{
			"missing/file": {Kind: entryFile, ObjectRef: obj1, Size: 1},
		},
	}
	if _, err := inspectManifestEntries("manifest.json", &manifest); err == nil || !strings.Contains(err.Error(), "missing parent directory") {
		t.Fatalf("validation error = %v, want missing parent directory", err)
	}

	manifest = manifestProjection{
		SourceUDID: "testphoneudid0001", SizeBytes: 2,
		Entries: map[string]manifestEntry{
			"parent":       {Kind: entryFile, ObjectRef: obj1, Size: 1},
			"parent/child": {Kind: entryFile, ObjectRef: obj2, Size: 1},
		},
	}
	if _, err := inspectManifestEntries("manifest.json", &manifest); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("validation error = %v, want non-directory parent", err)
	}
}

func TestManifestRejectsExcessiveLogicalDepth(t *testing.T) {
	manifest := manifestProjection{
		Entries: map[string]manifestEntry{
			strings.Repeat("a/", maxLogicalDepth) + "a": {Kind: entryDirectory},
		},
	}
	if _, err := inspectManifestEntries("manifest.json", &manifest); err == nil || !strings.Contains(err.Error(), "exceeds 128 components") {
		t.Fatalf("validation error = %v, want excessive depth", err)
	}
}

func TestOpenSnapshotRejectsSealedEntryMutation(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	const source = "testphoneudid0001"
	entries := map[string]manifestEntry{
		"Manifest.db": {Kind: entryFile, ObjectRef: obj1, Size: 100},
	}
	writeTestManifest(t, root, source, genA, entries)
	if _, err := store.OpenSnapshot(source, genA); err != nil {
		t.Fatalf("open sealed manifest: %v", err)
	}
	// Rewrite the manifest keeping the original seal but a mutated entry.
	seal, err := entriesChecksum(&manifestProjection{Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	entries["Manifest.db"] = manifestEntry{Kind: entryFile, ObjectRef: obj2, Size: 100}
	manifest := manifestProjection{
		Version: formatVersion, SourceUDID: source,
		SnapshotID: genA, CreatedUnix: 1, SizeBytes: 100,
		EntriesSHA256: seal, Entries: entries,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, source, "snapshots", genA+".json")
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenSnapshot(source, genA); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("mutated manifest error = %v, want checksum mismatch", err)
	}
}
