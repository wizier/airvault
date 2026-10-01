package objectstore

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// newTarTestSnapshot publishes a Finder-shaped snapshot — top-level files, a
// two-hex folder, an empty file and a path long enough to need a PAX header —
// and returns it with its file contents.
func newTarTestSnapshot(t *testing.T) (*Snapshot, map[string]string) {
	t.Helper()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginSnapshot(draftSource, snapFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Manifest.db":                    strings.Repeat("sqlite", 200),
		"ab/abcdef012":                   "file content",
		"ab/empty":                       "",
		"ab/" + strings.Repeat("x", 150): "long",
	}
	for key, content := range files {
		writeKey(t, session, key, content)
	}
	snapshot := publish(t, store, session)
	return snapshot, files
}

func TestTarPacksSnapshotUnderRootWithExactSize(t *testing.T) {
	snapshot, files := newTarTestSnapshot(t)
	archive, err := snapshot.Tar("backup")
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
	snapshot, files := newTarTestSnapshot(t)
	object, err := snapshot.store.resolveObjectRef(snapshot.Source(), refOf(files["ab/abcdef012"]))
	if err != nil {
		t.Fatal(err)
	}
	// A whole read and a download resumed mid-file both catch it and set it aside.
	for _, resume := range []bool{false, true} {
		// Same size, different bytes: only the hash can tell.
		if err := os.WriteFile(object, []byte("file CONTENT"), 0o644); err != nil {
			t.Fatal(err)
		}
		archive, err := snapshot.Tar("backup")
		if err != nil {
			t.Fatal(err)
		}
		if resume {
			index := slices.Index(archive.paths, "ab/abcdef012")
			size := int64(len(files["ab/abcdef012"]))
			contentStart := archive.offsets[index+1] - tarPadding(size) - size
			if _, err := archive.Seek(contentStart+5, io.SeekStart); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := io.ReadAll(archive); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("resume %v: read error = %v, want a hash mismatch", resume, err)
		}
		archive.Close()
		requirePath(t, object, false)
		requirePath(t, object+damagedSuffix, true)
	}
}

// Every offset reads what a whole-archive read has there, so any Range a
// resumed download asks for lands on the right bytes.
func TestTarSeeksToAnyOffset(t *testing.T) {
	snapshot, _ := newTarTestSnapshot(t)
	archive, err := snapshot.Tar("backup")
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
