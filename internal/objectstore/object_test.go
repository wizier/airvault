package objectstore

import (
	"errors"
	"os"
	"testing"
)

// A missing object is damage; one that fails to open says nothing of its
// content and stays in the pool.
func TestOpenTellsDamageFromFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root opens a file whatever its mode")
	}
	openers := map[string]func(*Snapshot, string) error{
		"Open":     func(snapshot *Snapshot, key string) error { _, err := snapshot.Open(key); return err },
		"OpenFile": func(snapshot *Snapshot, key string) error { _, err := snapshot.OpenFile(key); return err },
	}
	for name, open := range openers {
		t.Run(name, func(t *testing.T) {
			store, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			draft, err := store.BeginSnapshot(draftSource, snapFull, nil)
			if err != nil {
				t.Fatal(err)
			}
			writeKey(t, draft, "missing", "gone from the pool")
			writeKey(t, draft, "denied", "kept in the pool")
			snapshot := publish(t, store, draft)
			missing, _ := store.resolveObjectRef(draftSource, refOf("gone from the pool"))
			denied, _ := store.resolveObjectRef(draftSource, refOf("kept in the pool"))
			if err := os.Remove(missing); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(denied, 0); err != nil {
				t.Fatal(err)
			}

			if err := open(snapshot, "missing"); !errors.Is(err, ErrIntegrity) {
				t.Errorf("a missing object: %v, want ErrIntegrity", err)
			}
			if err := open(snapshot, "denied"); err == nil || errors.Is(err, ErrIntegrity) {
				t.Errorf("an object that failed to open: %v, want a failure that is not ErrIntegrity", err)
			}
			requirePath(t, denied, true)
		})
	}
}
