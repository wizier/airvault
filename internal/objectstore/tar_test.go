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
	defer archive.Close()
	out, err := io.ReadAll(archive)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(out)) != archive.Size() {
		t.Fatalf("read %d bytes, promised %d", len(out), archive.Size())
	}

	reader := tar.NewReader(bytes.NewReader(out))
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
	defer archive.Close()
	if _, err := io.ReadAll(archive); err == nil || !strings.Contains(err.Error(), "does not match its hash") {
		t.Fatalf("read error = %v, want a hash mismatch", err)
	}
}

// Every offset reads what a whole-archive read has there, so any Range a
// resumed download asks for lands on the right bytes.
func TestTarSeeksToAnyOffset(t *testing.T) {
	view, _, _ := newTarTestSnapshot(t)
	archive, err := view.Tar("backup")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	full, err := io.ReadAll(archive)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 700)
	for offset := range int64(len(full)) {
		if _, err := archive.Seek(offset, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		want := full[offset:min(offset+int64(len(chunk)), int64(len(full)))]
		got := chunk[:len(want)]
		if _, err := io.ReadFull(archive, got); err != nil {
			t.Fatalf("offset %d: %v", offset, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("offset %d: bytes differ from the whole-archive read", offset)
		}
	}
}
