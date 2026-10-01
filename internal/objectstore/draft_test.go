package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const (
	draftSource = "testphoneudid0009"
	snapFull    = "11111111-1111-4111-8111-111111111111"
	snapNext    = "22222222-2222-4222-8222-222222222222"
)

func writeKey(t *testing.T, draft *Draft, key, content string) {
	t.Helper()
	writer, err := draft.Create(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
}

func readKey(t *testing.T, draft *Draft, key string) string {
	t.Helper()
	reader, err := draft.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func refOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// publish seals a draft and publishes it the way the service does.
func publish(t *testing.T, store *Store, draft *Draft) *Snapshot {
	t.Helper()
	staged, err := draft.Seal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.Publish(staged)
	if err != nil {
		t.Fatal(err)
	}
	return published
}

func TestDraftWritesAndPublishesASnapshot(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	draft, err := store.BeginSnapshot(draftSource, snapFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := draft.Open("Status.plist"); !errors.Is(err, fs.ErrNotExist) || draft.Err() != nil {
		t.Fatalf("missing key: %v, latched %v", err, draft.Err())
	}
	if err := draft.MakeDirAll("ab"); err != nil {
		t.Fatal(err)
	}
	writeKey(t, draft, "ab/one", "same bytes")
	writeKey(t, draft, "ab/two", "same bytes")
	writeKey(t, draft, "Manifest.db", "database")
	if readKey(t, draft, "ab/two") != "same bytes" {
		t.Fatal("content read back differs")
	}
	if entries, err := draft.List("ab"); err != nil || len(entries) != 2 || entries[0].Name != "one" {
		t.Fatalf("List = %+v, %v", entries, err)
	}

	publish(t, store, draft)
	snapshot, err := store.OpenSnapshot(draftSource, snapFull)
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := snapshot.List("ab"); err != nil || len(entries) != 2 || entries[0].Name != "one" || entries[0].Modified.IsZero() {
		t.Fatalf("published List = %+v, %v", entries, err)
	}
	if !snapshot.Exists("") || !snapshot.Exists("ab/one") || snapshot.Exists("ab/three") {
		t.Fatal("published Exists disagrees with the manifest")
	}
	if one, two := snapshot.manifest.Entries["ab/one"], snapshot.manifest.Entries["ab/two"]; one.ObjectRef != two.ObjectRef || one.ObjectRef != refOf("same bytes") {
		t.Fatal("identical content must share one object")
	}
	if snapshot.SizeBytes() != int64(2*len("same bytes")+len("database")) {
		t.Fatalf("size = %d: every entry counts", snapshot.SizeBytes())
	}
	if _, err := store.BeginSnapshot(draftSource, snapFull, nil); err == nil {
		t.Fatal("a snapshot id was reused")
	}
}

// An incremental backup inherits its base; objects it wrote and then dropped
// go with the collection that follows its publication.
func TestIncrementalDraftInheritsAndPrunes(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.BeginSnapshot(draftSource, snapFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, base, "Manifest.db", "v1")
	writeKey(t, base, "photo", "picture")
	published := publish(t, store, base)

	next, err := store.BeginSnapshot(draftSource, snapNext, published)
	if err != nil {
		t.Fatal(err)
	}
	if readKey(t, next, "photo") != "picture" {
		t.Fatal("the base's content is not inherited")
	}
	writeKey(t, next, "Manifest.db", "v2-draft")
	writeKey(t, next, "Manifest.db", "v2")
	publish(t, store, next)
	if _, err := collectAll(t, store, draftSource); err != nil {
		t.Fatal(err)
	}
	draft, _ := store.resolveObjectRef(draftSource, refOf("v2-draft"))
	if _, err := os.Stat(draft); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("an object the snapshot dropped was not collected")
	}
	old, _ := store.resolveObjectRef(draftSource, refOf("v1"))
	if _, err := os.Stat(old); err != nil {
		t.Fatal("collection touched an object the base snapshot still uses")
	}
}

// Same-length damage a read's hash catches is latched and set aside, as Verify
// sets aside what no one read and puts back what reads right again. A writer
// of the same content puts a whole copy back, as it heals a truncated one.
func TestDraftVerifiesAndHealsObjects(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.BeginSnapshot(draftSource, snapFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, base, "file", "original")
	published := publish(t, store, base)
	path, _ := store.resolveObjectRef(draftSource, refOf("original"))

	if err := os.WriteFile(path, []byte("damaged!"), 0o644); err != nil {
		t.Fatal(err)
	}
	reader, _ := published.Open("file")
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("damaged object: %v, want ErrIntegrity", err)
	}
	requirePath(t, path+damagedSuffix, true)

	verify := func(wantSetAside int, wantDamage string) {
		t.Helper()
		ctx := context.Background()
		scan, err := store.Scan(ctx, draftSource)
		if err != nil {
			t.Fatal(err)
		}
		if setAside, err := store.Verify(ctx, scan, func(int64, int64) {}); setAside != wantSetAside || err != nil {
			t.Fatalf("Verify = %d, %v; want %d set aside", setAside, err, wantSetAside)
		}
		if scan, err = store.Scan(ctx, draftSource); err != nil || scan.Snapshots[0].Damage != wantDamage {
			t.Fatalf("health = %+v, %v; want damage %q", scan.Snapshots, err, wantDamage)
		}
	}
	verify(0, DamageFilesMissing)
	if err := os.WriteFile(path+damagedSuffix, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	verify(0, "")
	if err := os.WriteFile(path, []byte("damaged!"), 0o644); err != nil {
		t.Fatal(err)
	}
	verify(1, DamageFilesMissing)

	// The object set aside comes back with a writer of the same content.
	next, err := store.BeginSnapshot(draftSource, snapNext, published)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, next, "again", "original")
	if data, _ := os.ReadFile(path); string(data) != "original" {
		t.Fatalf("lost object = %q, want a whole copy back", data)
	}
	if err := os.WriteFile(path, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeKey(t, next, "once more", "original")
	if data, _ := os.ReadFile(path); string(data) != "original" {
		t.Fatalf("truncated object = %q, want it healed", data)
	}
}

func TestDraftAbortAndOpenWriters(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	draft, err := store.BeginSnapshot(draftSource, snapFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	aborted, _ := draft.Create("partial")
	_, _ = io.WriteString(aborted, "half a file")
	aborted.Abort()
	if draft.Exists("partial") {
		t.Fatal("an aborted file was filed")
	}
	temps, _ := filepath.Glob(filepath.Join(draft.staging, "objects", "*"))
	if len(temps) != 0 {
		t.Fatalf("an aborted file left %v", temps)
	}
	open, _ := draft.Create("open")
	if _, err := draft.Seal(context.Background()); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Seal with an open writer: %v", err)
	}
	open.Abort()
}

func TestDraftNamespaceOperations(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	draft, err := store.BeginSnapshot(draftSource, snapFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, draft, "a/file", "content")
	if err := draft.Copy("missing", "b"); err != nil || draft.Exists("b") {
		t.Fatalf("copying a missing source must be a silent no-op: %v", err)
	}
	if err := draft.Copy("a", "a/inner"); err != nil || draft.Exists("a/inner") {
		t.Fatalf("copying into itself must be a silent no-op: %v", err)
	}
	if err := draft.Copy("a", "b"); err != nil || readKey(t, draft, "b/file") != "content" {
		t.Fatalf("Copy = %v", err)
	}
	if err := draft.Rename("b", "c"); err != nil || draft.Exists("b") || readKey(t, draft, "c/file") != "content" {
		t.Fatalf("Rename = %v", err)
	}
	if err := draft.Remove("c"); err != nil || draft.Exists("c/file") {
		t.Fatalf("Remove = %v", err)
	}
	if err := draft.Remove("c"); err == nil {
		t.Fatal("removing a missing path succeeded")
	}
	if err := draft.MakeDirAll("a/file/sub"); err == nil {
		t.Fatal("a directory was made through a file")
	}
	if names := entryNames(t, draft, ""); !slices.Equal(names, []string{"a"}) {
		t.Fatalf("root = %v", names)
	}
	if draft.Err() != nil {
		t.Fatalf("refused requests must not latch: %v", draft.Err())
	}
}

func entryNames(t *testing.T, draft *Draft, key string) []string {
	t.Helper()
	entries, err := draft.List(key)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

// An empty file is checked too: its reference must be the empty content's.
func TestDraftVerifiesEmptyFiles(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	writeTestManifest(t, root, draftSource, snapFull, map[string]manifestEntry{
		"empty": {Kind: entryFile, ObjectRef: obj1, Size: 0},
	})
	writeTestObject(t, root, draftSource, obj1, 0)
	snapshot, err := store.OpenSnapshot(draftSource, snapFull)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := snapshot.Open("empty")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("empty file under a wrong reference: %v, want ErrIntegrity", err)
	}
}

// A base snapshot seeds the tree of its own source only: objects are pooled
// per source, so another source's would be unreachable.
func TestBeginSnapshotRefusesAnotherSourcesBase(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.BeginSnapshot(draftSource, snapFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	published := publish(t, store, base)
	if _, err := store.BeginSnapshot("otherphoneudid0001", snapNext, published); err == nil {
		t.Fatal("a snapshot began from another source's base")
	}
}
