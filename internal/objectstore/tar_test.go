package objectstore

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTarTestSnapshot publishes a Finder-shaped snapshot — top-level files, a
// two-hex folder, an empty file and a path long enough to need a PAX header —
// and returns it with its file contents and object folder.
func newTarTestSnapshot(t *testing.T) (*View, map[string]string, string) {
	t.Helper()
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	const source = "testphoneudid0001"
	files := map[string]string{
		"Manifest.db":                    strings.Repeat("sqlite", 200),
		"ab/abcdef012":                   "file content",
		"ab/empty":                       "",
		"ab/" + strings.Repeat("x", 150): "long",
	}
	entries := map[string]manifestEntry{"ab": {Kind: entryDirectory}}
	objects := filepath.Join(root, source, "objects")
	for name, body := range files {
		sum := sha256.Sum256([]byte(body))
		ref := hex.EncodeToString(sum[:])
		if err := os.MkdirAll(filepath.Join(objects, ref[:2]), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(objects, ref[:2], ref), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		entries[name] = manifestEntry{Kind: entryFile, ObjectRef: ref, Size: int64(len(body))}
	}
	writeTestManifest(t, root, source, genA, entries)
	view, err := store.OpenSnapshot(source, genA)
	if err != nil {
		t.Fatal(err)
	}
	return view, files, objects
}

func TestTarPacksSnapshotUnderRootWithExactSize(t *testing.T) {
	view, files, _ := newTarTestSnapshot(t)
	archive, err := view.Tar("backup")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	written, err := archive.WriteTo(&out)
	if err != nil {
		t.Fatal(err)
	}
	if written != archive.Size() || int64(out.Len()) != archive.Size() {
		t.Fatalf("wrote %d (%d buffered), promised %d", written, out.Len(), archive.Size())
	}

	reader := tar.NewReader(&out)
	var names []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if want, ok := files[strings.TrimPrefix(header.Name, "backup/")]; ok && string(body) != want {
			t.Errorf("%s: content %q, want %q", header.Name, body, want)
		}
	}
	want := []string{"backup/", "backup/Manifest.db", "backup/ab/", "backup/ab/abcdef012",
		"backup/ab/empty", "backup/ab/" + strings.Repeat("x", 150)}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("entries:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
}

func TestTarFailsOnDamagedObject(t *testing.T) {
	view, files, objects := newTarTestSnapshot(t)
	sum := sha256.Sum256([]byte(files["ab/abcdef012"]))
	ref := hex.EncodeToString(sum[:])
	// Same size, different bytes: only the hash can tell.
	if err := os.WriteFile(filepath.Join(objects, ref[:2], ref), []byte("file CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, err := view.Tar("backup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.WriteTo(io.Discard); err == nil || !strings.Contains(err.Error(), "does not match its hash") {
		t.Fatalf("WriteTo error = %v, want a hash mismatch", err)
	}
}
