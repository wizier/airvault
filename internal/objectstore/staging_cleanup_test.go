package objectstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A manifest that no longer decodes is the catalog reconciler's problem. It
// must not keep the store from starting, and its envelope must not survive to
// block collection.
func TestReconcileStagingClearsEnvelopeOfUnreadableManifest(t *testing.T) {
	store, source := newTestStore(t)
	stagingDir := filepath.Join(store.root, source, "staging", genA)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(store.root, source, "snapshots", genA+".json")
	if err := os.WriteFile(manifestPath, []byte(`{"broken":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.ReconcileSourceStaging(source); err != nil {
		t.Fatalf("unreadable manifest blocked staging reconciliation: %v", err)
	}
	if _, err := os.Stat(stagingDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging envelope survived: %v", err)
	}
	if err := store.ensureCollectable(source); err != nil {
		t.Fatalf("staging still blocks collection: %v", err)
	}
}

func TestDiscardStagingLeavesPooledOrphanForMarkAndSweep(t *testing.T) {
	store, source := newTestStore(t)
	writeTestObject(t, store.root, source, obj3, 40)
	pooled, err := store.resolveObjectRef(source, obj3)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(store.root, source, "staging", genC, "objects", "random.tmp")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(pooled, staged); err != nil {
		t.Fatal(err)
	}

	if err := store.DiscardStaging(source, genC); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging object survived discard: %v", err)
	}
	if _, err := os.Stat(pooled); err != nil {
		t.Fatalf("pooled orphan disappeared before GC: %v", err)
	}
	if _, err := collectAll(t, store, source); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pooled); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("pooled orphan survived mark-and-sweep: %v", err)
	}
}

func stageTestManifest(t *testing.T, store *Store, source, snapshotID string, entries map[string]manifestEntry) (string, string) {
	t.Helper()
	writeTestManifest(t, store.root, source, snapshotID, entries)
	stagingPath := filepath.Join(store.root, source, "staging", snapshotID, "manifest.json")
	if err := os.MkdirAll(filepath.Dir(stagingPath), 0o755); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(store.root, source, "snapshots", snapshotID+".json")
	if err := os.Rename(finalPath, stagingPath); err != nil {
		t.Fatal(err)
	}
	return stagingPath, finalPath
}

func openTestStaging(t *testing.T, store *Store, source, snapshotID string) *StagingView {
	t.Helper()
	staging, err := store.OpenStaging(source, snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	return staging
}

// Publication references pooled content-addressed objects, clears the staging
// envelope and returns a view identical to the final manifest read back.
func TestPublishCommitsStagedManifestOverSharedPool(t *testing.T) {
	store, source := newTestStore(t)
	entries := map[string]manifestEntry{
		"inherited": {Kind: entryFile, ObjectRef: obj1, Size: 100},
		"new":       {Kind: entryFile, ObjectRef: obj3, Size: 40},
	}
	stagingPath, _ := stageTestManifest(t, store, source, genC, entries)
	writeTestObject(t, store.root, source, obj3, 40)
	published, err := store.Publish(openTestStaging(t, store, source, genC))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.OpenSnapshot(source, genC)
	if err != nil {
		t.Fatalf("open published snapshot: %v", err)
	}
	if published.CreatedUnix() != reopened.CreatedUnix() || published.SizeBytes() != reopened.SizeBytes() {
		t.Fatalf("published view diverges from final manifest: (%d,%d) vs (%d,%d)",
			published.CreatedUnix(), published.SizeBytes(), reopened.CreatedUnix(), reopened.SizeBytes())
	}
	if _, err := os.Stat(filepath.Dir(stagingPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging directory survived publication: %v", err)
	}
	// Completing an already-completed publication is a safe no-op (recovery).
	if err := store.FinishPublication(published); err != nil {
		t.Fatal(err)
	}
}

func TestPublishSyncsContentsBeforeFinalRename(t *testing.T) {
	store, source := newTestStore(t)
	entries := map[string]manifestEntry{"inherited": {Kind: entryFile, ObjectRef: obj1, Size: 100}}
	stagingPath, finalPath := stageTestManifest(t, store, source, genC, entries)

	injected := errors.New("injected content sync failure")
	store.syncContents = func() error { return injected }
	if _, err := store.Publish(openTestStaging(t, store, source, genC)); !errors.Is(err, injected) {
		t.Fatalf("Publish error = %v, want injected sync failure", err)
	}
	if _, err := os.Stat(finalPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("final manifest became visible before content sync: %v", err)
	}
	if _, err := os.Stat(stagingPath); err != nil {
		t.Fatalf("staging manifest was not preserved: %v", err)
	}
}

func TestFinishPublicationCompletesDurabilityAfterFinalRename(t *testing.T) {
	store, source := newTestStore(t)
	entries := map[string]manifestEntry{"inherited": {Kind: entryFile, ObjectRef: obj1, Size: 100}}
	stageTestManifest(t, store, source, genC, entries)

	injected := errors.New("injected directory sync failure")
	snapshotSyncs := 0
	store.syncDir = func(directory string) error {
		if filepath.Base(directory) == "snapshots" {
			snapshotSyncs++
			if snapshotSyncs == 1 {
				return injected
			}
		}
		return syncDirectory(directory)
	}
	if _, err := store.Publish(openTestStaging(t, store, source, genC)); !errors.Is(err, injected) {
		t.Fatalf("first Publish error = %v, want injected sync failure", err)
	}
	// The rename crossed before the injected failure: recovery reopens the
	// published manifest and completes the same directory commit.
	published, err := store.OpenSnapshot(source, genC)
	if err != nil {
		t.Fatalf("rename did not precede injected failure: %v", err)
	}
	if err := store.FinishPublication(published); err != nil {
		t.Fatalf("FinishPublication: %v", err)
	}
	if snapshotSyncs != 2 {
		t.Fatalf("snapshot directory syncs = %d, want retry after rename", snapshotSyncs)
	}
	if _, err := os.Stat(filepath.Join(store.root, source, "staging", genC)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging envelope survived recovery: %v", err)
	}
}
