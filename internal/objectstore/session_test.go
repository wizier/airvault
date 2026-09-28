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
	sessionSource = "testphoneudid0009"
	snapFull      = "11111111-1111-4111-8111-111111111111"
	snapNext      = "22222222-2222-4222-8222-222222222222"
)

func writeKey(t *testing.T, session *Session, key, content string) {
	t.Helper()
	writer, err := session.Create(key)
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

func readKey(t *testing.T, session *Session, key string) string {
	t.Helper()
	reader, err := session.Open(key)
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

// publish seals a session and publishes it the way the service does.
func publish(t *testing.T, store *Store, session *Session) int64 {
	t.Helper()
	added, err := session.Seal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staging, err := store.OpenStaging(session.source, session.snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(staging); err != nil {
		t.Fatal(err)
	}
	return added
}

func TestSessionWritesAndPublishesASnapshot(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginSnapshot(sessionSource, snapFull, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Open("Status.plist"); !errors.Is(err, fs.ErrNotExist) || session.Err() != nil {
		t.Fatalf("missing key: %v, latched %v", err, session.Err())
	}
	if err := session.MakeDirAll("ab"); err != nil {
		t.Fatal(err)
	}
	writeKey(t, session, "ab/one", "same bytes")
	writeKey(t, session, "ab/two", "same bytes")
	writeKey(t, session, "Manifest.db", "database")
	writeKey(t, session, ProtocolDir+"/.b/staged", "device scratch")
	if readKey(t, session, "ab/two") != "same bytes" {
		t.Fatal("content read back differs")
	}
	if entries, err := session.List("ab"); err != nil || len(entries) != 2 || entries[0].Name != "one" {
		t.Fatalf("List = %+v, %v", entries, err)
	}

	if added := publish(t, store, session); added != int64(len("same bytes")+len("database")) {
		t.Fatalf("added = %d: duplicates count once, protocol files not at all", added)
	}
	view, err := store.OpenSnapshot(sessionSource, snapFull)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := view.manifest.Entries[ProtocolDir]; has {
		t.Fatal("the protocol directory reached the sealed snapshot")
	}
	if one, two := view.manifest.Entries["ab/one"], view.manifest.Entries["ab/two"]; one.ObjectRef != two.ObjectRef || one.ObjectRef != refOf("same bytes") {
		t.Fatal("identical content must share one object")
	}
	if view.SizeBytes() != int64(2*len("same bytes")+len("database")) {
		t.Fatalf("size = %d: every entry counts", view.SizeBytes())
	}
	if _, err := store.BeginSnapshot(sessionSource, snapFull, ""); err == nil {
		t.Fatal("a snapshot id was reused")
	}
}

// An incremental backup inherits its base; objects it wrote and then
// dropped are pruned, and only surviving new content counts as added.
func TestIncrementalSessionInheritsAndPrunes(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.BeginSnapshot(sessionSource, snapFull, "")
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, base, "Manifest.db", "v1")
	writeKey(t, base, "photo", "picture")
	publish(t, store, base)

	next, err := store.BeginSnapshot(sessionSource, snapNext, snapFull)
	if err != nil {
		t.Fatal(err)
	}
	if readKey(t, next, "photo") != "picture" {
		t.Fatal("the base's content is not inherited")
	}
	writeKey(t, next, "Manifest.db", "v2-draft")
	writeKey(t, next, "Manifest.db", "v2")
	if added := publish(t, store, next); added != int64(len("v2")) {
		t.Fatalf("added = %d, want only the surviving new object", added)
	}
	draft, _ := store.resolveObjectRef(sessionSource, refOf("v2-draft"))
	if _, err := os.Stat(draft); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("an object the snapshot dropped was not pruned")
	}
	old, _ := store.resolveObjectRef(sessionSource, refOf("v1"))
	if _, err := os.Stat(old); err != nil {
		t.Fatal("prune touched an object the base snapshot still uses")
	}
}

// Same-length damage is caught by the hash at the last byte and latched;
// a writer of the same content heals a truncated object.
func TestSessionVerifiesAndHealsObjects(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.BeginSnapshot(sessionSource, snapFull, "")
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, base, "file", "original")
	publish(t, store, base)
	path, _ := store.resolveObjectRef(sessionSource, refOf("original"))

	if err := os.WriteFile(path, []byte("damaged!"), 0o644); err != nil {
		t.Fatal(err)
	}
	restore, err := store.OpenRestore(sessionSource, snapFull)
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := restore.Open("file")
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrIntegrity) || !errors.Is(restore.Err(), ErrIntegrity) {
		t.Fatalf("damaged object: %v, latched %v", err, restore.Err())
	}
	if _, err := restore.Create("x"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("restore write: %v, want ErrReadOnly", err)
	}

	if err := os.WriteFile(path, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	next, err := store.BeginSnapshot(sessionSource, snapNext, snapFull)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, next, "again", "original")
	if data, _ := os.ReadFile(path); string(data) != "original" {
		t.Fatalf("truncated object = %q, want it healed", data)
	}
}

func TestSessionAbortAndOpenWriters(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginSnapshot(sessionSource, snapFull, "")
	if err != nil {
		t.Fatal(err)
	}
	aborted, _ := session.Create("partial")
	_, _ = io.WriteString(aborted, "half a file")
	aborted.Abort()
	if session.Exists("partial") {
		t.Fatal("an aborted file was filed")
	}
	temps, _ := filepath.Glob(filepath.Join(session.staging, "objects", "*"))
	if len(temps) != 0 {
		t.Fatalf("an aborted file left %v", temps)
	}
	open, _ := session.Create("open")
	if _, err := session.Seal(context.Background()); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Seal with an open writer: %v", err)
	}
	open.Abort()
}

func TestSessionNamespaceOperations(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginSnapshot(sessionSource, snapFull, "")
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, session, "a/file", "content")
	if err := session.Copy("missing", "b"); err != nil || session.Exists("b") {
		t.Fatalf("copying a missing source must be a silent no-op: %v", err)
	}
	if err := session.Copy("a", "a/inner"); err != nil || session.Exists("a/inner") {
		t.Fatalf("copying into itself must be a silent no-op: %v", err)
	}
	if err := session.Copy("a", "b"); err != nil || readKey(t, session, "b/file") != "content" {
		t.Fatalf("Copy = %v", err)
	}
	if err := session.Rename("b", "c"); err != nil || session.Exists("b") || !session.IsDir("c") {
		t.Fatalf("Rename = %v", err)
	}
	if err := session.Remove("c"); err != nil || session.Exists("c/file") {
		t.Fatalf("Remove = %v", err)
	}
	if err := session.Remove("c"); err == nil {
		t.Fatal("removing a missing path succeeded")
	}
	if err := session.MakeDirAll("a/file/sub"); err == nil {
		t.Fatal("a directory was made through a file")
	}
	if names := entryNames(t, session, ""); !slices.Equal(names, []string{"a"}) {
		t.Fatalf("root = %v", names)
	}
	if session.Err() != nil {
		t.Fatalf("refused requests must not latch: %v", session.Err())
	}
}

func entryNames(t *testing.T, session *Session, key string) []string {
	t.Helper()
	entries, err := session.List(key)
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
func TestSessionVerifiesEmptyFiles(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	writeTestManifest(t, root, sessionSource, snapFull, map[string]manifestEntry{
		"empty": {Kind: entryFile, ObjectRef: obj1, Size: 0},
	})
	writeTestObject(t, root, sessionSource, obj1, 0)
	restore, err := store.OpenRestore(sessionSource, snapFull)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := restore.Open("empty")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("empty file under a wrong reference: %v, want ErrIntegrity", err)
	}
}
