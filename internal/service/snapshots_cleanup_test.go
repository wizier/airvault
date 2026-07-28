package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"
	airvault "github.com/wizier/airvault"
	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
	"github.com/wizier/airvault/internal/storage"
	_ "modernc.org/sqlite"
)

const cleanupTestSnapshot = "cccccccc-0000-4000-8000-000000000003"

func newSnapshotCleanupService(t *testing.T) (*Service, string) {
	t.Helper()
	db, err := sqlx.Open("sqlite", filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	goose.SetBaseFS(airvault.MigrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(db.DB, "migrations"); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	objects, err := objectstore.New(root)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		store:   storage.NewStore(db),
		objects: objects,
		bus:     events.New(),
		live:    newDeviceRuntimeStore(),
		ops:     newOperationManager(),
	}
	t.Cleanup(func() {
		_ = objects.Close()
		_ = db.Close()
	})
	return svc, root
}

func writeUnpublishedStagingState(t *testing.T, root, source string) (string, string) {
	t.Helper()
	stagingDir := filepath.Join(root, source, "staging", cleanupTestSnapshot)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json.tmp"), []byte(`{"incomplete":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	objectDir := filepath.Join(stagingDir, "objects")
	if err := os.MkdirAll(objectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objectDir, "random.tmp"), []byte("unfinished"), 0o644); err != nil {
		t.Fatal(err)
	}
	return stagingDir, objectDir
}

// The minimum an iOS backup must contain to pass validation. Constant, so the
// entries seal below is constant too and only the snapshot's identity varies.
var fixtureFiles = map[string]string{
	"Manifest.plist": `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>IsEncrypted</key><false/>
	<key>Lockdown</key><dict>
		<key>ProductVersion</key><string>18.0</string>
	</dict>
</dict></plist>
`,
	"Status.plist": `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>IsFullBackup</key><true/>
	<key>SnapshotState</key><string>finished</string>
</dict></plist>
`,
	"Info.plist": `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Device Name</key><string>Test iPhone</string>
	<key>Product Type</key><string>iPhone16,2</string>
	<key>Target Identifier</key><string>testphoneudid0001</string>
</dict></plist>
`,
	"Manifest.db": "sqlite fixture",
}

const (
	fixtureCreatedUnix = 1_700_000_000
	// Pinned wire value: SHA-256 over the sealed encoding of the entries above.
	fixtureEntriesSeal = "94c2bd3ab8d1852db77b4e1439c3ccd3dc3809cd075b0afc9af2422ca71a8f5b"
)

// writePublishedSnapshot lays down a snapshot the object store accepts as
// published: one content-addressed object per file, then their sealed manifest.
// It returns the object paths followed by the manifest path.
func writePublishedSnapshot(t *testing.T, root, source, snapshotID string) []string {
	t.Helper()
	entries := make(map[string]any, len(fixtureFiles))
	paths := make([]string, 0, len(fixtureFiles)+1)
	var sizeBytes int64
	for name, body := range fixtureFiles {
		ref := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
		path := filepath.Join(root, source, "objects", ref[:2], ref)
		writeTestFile(t, path, []byte(body))
		paths = append(paths, path)
		sizeBytes += int64(len(body))
		entries[name] = map[string]any{"kind": "file", "objectRef": ref, "size": len(body)}
	}
	manifest, err := json.Marshal(map[string]any{
		"version": 1, "sourceUdid": source, "snapshotId": snapshotID,
		"createdUnix": fixtureCreatedUnix, "sizeBytes": sizeBytes,
		"entriesSha256": fixtureEntriesSeal, "entries": entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, source, "snapshots", snapshotID+".json")
	writeTestFile(t, manifestPath, manifest)
	return append(paths, manifestPath)
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireCleanupPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("path %s still exists or could not be inspected: %v", path, err)
	}
}

func pendingCleanupSnapshot(source string) *model.Backup {
	startedAt := int64(1_699_999_000)
	return &model.Backup{
		ID: cleanupTestSnapshot, SourceUDID: source, StartedAt: &startedAt,
	}
}

func TestStartupDiscardsUnpublishedAttemptAndNextSnapshotStartsFresh(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0003"
	stagingDir, objectDir := writeUnpublishedStagingState(t, root, source)
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}

	fresh, baseSnapshotID, err := svc.prepareSnapshot(context.Background(), &runReservation{udid: source})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == cleanupTestSnapshot {
		t.Fatalf("prepareSnapshot reused interrupted snapshot %s", fresh.ID)
	}
	if baseSnapshotID != "" {
		t.Fatalf("base snapshot = %q, want none", baseSnapshotID)
	}
	if _, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unpublished snapshot entered the catalog: %v", err)
	}
	if _, err := svc.store.Backup.Get(context.Background(), fresh.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("fresh mutable snapshot entered the catalog: %v", err)
	}
	requireCleanupPathAbsent(t, stagingDir)
	requireCleanupPathAbsent(t, objectDir)
}

func TestStartupRecoversPublishedAttemptAsIncrementalBase(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0004"
	publishedPaths := writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	stagingDir := filepath.Join(root, source, "staging", cleanupTestSnapshot)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}

	fresh, baseSnapshotID, err := svc.prepareSnapshot(context.Background(), &runReservation{udid: source})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == cleanupTestSnapshot {
		t.Fatalf("prepareSnapshot reused published snapshot %s", fresh.ID)
	}
	if baseSnapshotID != cleanupTestSnapshot {
		t.Fatalf("base snapshot = %q, want recovered %q", baseSnapshotID, cleanupTestSnapshot)
	}
	requireCleanupPathAbsent(t, stagingDir)
	recovered, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.CreatedAt == 0 {
		t.Fatalf("recovered snapshot has no creation time: %+v", recovered)
	}
	for _, path := range publishedPaths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("published data was removed at %s: %v", path, err)
		}
	}
}

// Deleting a device never reads a manifest and never asks the collector what it
// recognises: whatever the source left behind, the tree goes.
func TestDeleteBackupSourceRemovesSourceWhateverItLeftBehind(t *testing.T) {
	for _, test := range []struct {
		name     string
		source   string
		leftover func(t *testing.T, root, source string)
	}{
		{
			name:   "unpublished staging",
			source: "testphoneudid0002",
			leftover: func(t *testing.T, root, source string) {
				writeUnpublishedStagingState(t, root, source)
			},
		},
		{
			// A collection refuses a pool it cannot recognise; the tree goes anyway.
			name:   "unrecognised object pool",
			source: "testphoneudid0013",
			leftover: func(t *testing.T, root, source string) {
				objectsRoot := filepath.Join(root, source, "objects")
				if err := os.MkdirAll(objectsRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				stray := filepath.Join(objectsRoot, "not-an-object-prefix")
				if err := os.WriteFile(stray, []byte("junk"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "unreadable manifest",
			source: "testphoneudid0010",
			leftover: func(t *testing.T, root, source string) {
				manifestPath := filepath.Join(root, source, "snapshots", cleanupTestSnapshot+".json")
				if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(manifestPath, []byte(`{"broken":true}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, root := newSnapshotCleanupService(t)
			test.leftover(t, root, test.source)

			lease, err := svc.ops.acquire("test", snapshotWriteResource(test.source))
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.deleteBackupSource(context.Background(), lease, test.source); err != nil {
				t.Fatal(err)
			}
			// Reclamation runs in the background and owns the write lease.
			svc.wg.Wait()
			requireCleanupPathAbsent(t, filepath.Join(root, test.source))
			if _, err := svc.ops.acquire("test", snapshotWriteResource(test.source)); err != nil {
				t.Fatalf("write lease was not released after reclamation: %v", err)
			}
		})
	}
}

func TestDeletingTheLastSnapshotRemovesRowObjectsAndSourceTree(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0012"
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteSnapshots(context.Background(), source, []string{cleanupTestSnapshot}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("catalog row survived snapshot deletion: %v", err)
	}
	// Object collection runs in the background and owns the write lease. Nothing
	// is reachable afterwards, so it takes manifest, objects and directories
	// alike and the source stops being listed at every later startup.
	svc.wg.Wait()
	requireCleanupPathAbsent(t, filepath.Join(root, source))
	sources, err := svc.objects.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("emptied source is still listed: %v", sources)
	}
	if _, err := svc.ops.acquire("test", snapshotWriteResource(source)); err != nil {
		t.Fatalf("write lease was not released after background collection: %v", err)
	}
}

func TestDeleteSnapshotsRefreshesUsageCacheBeforeCollection(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0014"
	const keptID = "eeeeeeee-0000-4000-8000-000000000005"
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	writePublishedSnapshot(t, root, source, keptID)
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteSnapshots(context.Background(), source, []string{cleanupTestSnapshot}); err != nil {
		t.Fatal(err)
	}
	kept, err := svc.store.Backup.Get(context.Background(), keptID)
	if err != nil {
		t.Fatal(err)
	}
	manifestInfo, err := os.Stat(filepath.Join(root, source, "snapshots", keptID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	want := kept.SizeBytes + manifestInfo.Size()
	// The size is published within the request, before the sweep runs behind it.
	if got := sourceFootprint(t, svc, source); got == nil || *got != want {
		t.Fatalf("footprint after deletion = %v, want %d", got, want)
	}
	// The sweep works from the set the recount measured, so that size is final.
	svc.wg.Wait()
	if got := sourceFootprint(t, svc, source); got == nil || *got != want {
		t.Fatalf("footprint after collection = %v, want %d", got, want)
	}
}

// sourceFootprint reads the size the dashboard reads; nil means unknown.
func sourceFootprint(t *testing.T, svc *Service, source string) *int64 {
	t.Helper()
	summary, err := svc.store.Backup.SummaryBySource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return summary[source].DiskBytes
}

// publishSnapshot chooses between seeding the size, shifting it and leaving it
// unknown. Seeding one that was merely unknown would report less than what is
// really on disk, and nothing downstream would notice.
func TestPublishingSeedsThenShiftsButNeverInventsAFootprint(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0021"
	first, second := int64(1000), int64(250)
	publish := func(snapshotID string, added int64) {
		t.Helper()
		writePublishedSnapshot(t, root, source, snapshotID)
		view, err := svc.objects.OpenSnapshot(source, snapshotID)
		if err != nil {
			t.Fatal(err)
		}
		projection, err := snapshotProjection(view, source, snapshotID)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.publishSnapshot(context.Background(), projection, &added); err != nil {
			t.Fatal(err)
		}
	}

	publish("dddddddd-0000-4000-8000-00000000000a", first)
	if got := sourceFootprint(t, svc, source); got == nil || *got != first {
		t.Fatalf("seeded footprint = %v, want %d", got, first)
	}

	publish("dddddddd-0000-4000-8000-00000000000b", second)
	if got := sourceFootprint(t, svc, source); got == nil || *got != first+second {
		t.Fatalf("shifted footprint = %v, want %d", got, first+second)
	}

	if err := svc.store.Backup.ForgetSourceFootprint(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	publish("dddddddd-0000-4000-8000-00000000000c", second)
	if got := sourceFootprint(t, svc, source); got != nil {
		t.Fatalf("footprint on an unknown base = %d, want unknown", *got)
	}
}

// A duplicated device directory publishes one snapshot id twice. Startup keeps
// the first source and skips the copy rather than refusing to run.
func TestReconcileKeepsOneOwnerOfADuplicatedSnapshotID(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const kept, ignored = "testphoneudid0041", "testphoneudid0042"
	writePublishedSnapshot(t, root, kept, cleanupTestSnapshot)
	copied := writePublishedSnapshot(t, root, ignored, cleanupTestSnapshot)

	sources, err := svc.ReconcileBackupStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %v, want both", sources)
	}
	row, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if row.SourceUDID != kept {
		t.Fatalf("snapshot owner = %s, want %s", row.SourceUDID, kept)
	}
	// The copy stays on disk, so collection keeps its objects reachable.
	for _, path := range copied {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("ignored copy was removed: %v", err)
		}
	}
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if row, err = svc.store.Backup.Get(context.Background(), cleanupTestSnapshot); err != nil {
		t.Fatal(err)
	}
	if row.SourceUDID != kept {
		t.Fatalf("owner after the second pass = %s, want %s", row.SourceUDID, kept)
	}
}

func TestReconcileRebuildsCatalogAndBackupOnlyDeviceFromSnapshots(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0005"
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)

	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	backup, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if backup.SizeBytes <= 0 {
		t.Fatalf("rebuilt backup = %+v", backup)
	}
	if backup.TransferredBytes != nil {
		t.Fatalf("rebuilt transfer history = %d, want unknown", *backup.TransferredBytes)
	}
	if backup.StartedAt != nil {
		t.Fatalf("rebuilt start time = %d, want unknown", *backup.StartedAt)
	}
	if backup.CreatedAt != 1_700_000_000 {
		t.Fatalf("rebuilt creation time = %v, want manifest creation time", backup.CreatedAt)
	}
	devices, err := svc.DeviceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].UDID != source || devices[0].RestorePoints != 1 {
		t.Fatalf("backup-only devices = %+v", devices)
	}

	// Footprint is background maintenance, not part of catalog reconcile: drive one
	// scrub pass and let the sweep settle before asserting the cached size.
	svc.scrub(context.Background(), []string{source})
	svc.wg.Wait()
	devices, err = svc.DeviceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manifestInfo, err := os.Stat(filepath.Join(root, source, "snapshots", cleanupTestSnapshot+".json"))
	if err != nil {
		t.Fatal(err)
	}
	// The footprint counts live objects plus the published manifest itself.
	if want := backup.SizeBytes + manifestInfo.Size(); devices[0].DiskBytes == nil || *devices[0].DiskBytes != want {
		t.Fatalf("rebuilt disk bytes = %v, want %d", devices[0].DiskBytes, want)
	}
}

func TestReconcilePreservesFootprintOfSurvivingSource(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0013"
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.scrub(context.Background(), []string{source})
	svc.wg.Wait()
	devices, err := svc.DeviceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].DiskBytes == nil {
		t.Fatal("footprint not computed by scrub")
	}
	want := *devices[0].DiskBytes
	// A restart re-runs reconcile before the async scrub. It must not blank a
	// surviving source's footprint, or a source busy at scrub time shows an unknown
	// size until the next restart.
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	devices, err = svc.DeviceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].DiskBytes == nil || *devices[0].DiskBytes != want {
		t.Fatalf("reconcile blanked surviving footprint = %v, want %d", devices[0].DiskBytes, want)
	}
}

func TestReconcileReturnsEverySourceForVerification(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0015"
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	// Every source is returned for background verification, even when its
	// catalog and manifest IDs already agree.
	sources, err := svc.ReconcileBackupStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0] != source {
		t.Fatalf("first reconcile sources = %v, want [%s]", sources, source)
	}
	sources, err = svc.ReconcileBackupStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0] != source {
		t.Fatalf("unchanged reconcile sources = %v, want [%s]", sources, source)
	}
}

func TestStartupMaintenanceSkipsBusySourceAndCollectsOnNextPass(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0017"
	const abandonedID = "eeeeeeee-0000-4000-8000-000000000005"
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	const objectRef = "9999999999999999999999999999999999999999999999999999999999999999"
	staged := filepath.Join(root, source, "staging", abandonedID, "objects", "orphan.tmp")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("abandoned object"), 0o644); err != nil {
		t.Fatal(err)
	}
	pooled := filepath.Join(root, source, "objects", objectRef[:2], objectRef)
	if err := os.MkdirAll(filepath.Dir(pooled), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(staged, pooled); err != nil {
		t.Fatal(err)
	}
	// Reconcile drops the abandoned envelope but deliberately leaves its pooled
	// hard link for GC. The catalog/manifest IDs now agree, yet the source still
	// belongs in the complete startup pass.
	sources, err := svc.ReconcileBackupStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A busy source is left to its owner: the one-shot pass skips it without
	// touching the pool and does not retry.
	lease, err := svc.ops.acquire("test", snapshotWriteResource(source))
	if err != nil {
		t.Fatal(err)
	}
	svc.StartMaintenance(ctx, sources)
	svc.wg.Wait()
	if _, err := os.Stat(pooled); err != nil {
		t.Fatalf("busy source was swept instead of being deferred: %v", err)
	}
	lease.Release()

	// The next startup pass finds the source free and reclaims the orphan the
	// agreeing catalog/manifest IDs cannot rule out.
	svc.StartMaintenance(ctx, sources)
	svc.wg.Wait()
	requireCleanupPathAbsent(t, pooled)
}

func TestReclaimStopsWhenCorruptManifestCannotBeRemoved(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0019"
	paths := writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifestPath := paths[len(paths)-1]
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	// A directory is provably not a valid manifest, but RemoveSnapshot refuses
	// to unlink it. The incomplete live set must never reach CollectLive.
	if err := os.Mkdir(manifestPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.collectSource(context.Background(), source); err == nil {
		t.Fatal("reclaim succeeded without removing the corrupt manifest")
	}
	for _, objectPath := range paths[:len(paths)-1] {
		if _, err := os.Stat(objectPath); err != nil {
			t.Fatalf("published object was swept after manifest removal failed: %v", err)
		}
	}
}

func TestReconcileRemovesCatalogRowWhoseManifestDisappeared(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0006"
	paths := writePublishedSnapshot(t, root, source, cleanupTestSnapshot)

	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(paths[len(paths)-1]); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("catalog row survived missing manifest: %v", err)
	}
	devices, err := svc.DeviceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 0 {
		t.Fatalf("backup-only device survived missing manifest: %+v", devices)
	}
}

func TestReconcileDropsCorruptManifestNeverAdmitted(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0009"
	paths := writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	// Corrupt before the manifest is ever admitted: reconcile must not advertise it
	// and must unlink it so it stops pinning objects.
	if err := os.WriteFile(paths[len(paths)-1], []byte(`{"broken":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("corrupt manifest was advertised: %v", err)
	}
	if _, err := os.Stat(paths[len(paths)-1]); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("corrupt manifest still pins objects: %v", err)
	}
}

func TestAdmittedManifestTrustedAtReconcileThenDroppedByScrub(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0009"
	paths := writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Bit rot after admission: reconcile trusts the admitted manifest without
	// re-reading, so the restore point survives startup...
	if err := os.WriteFile(paths[len(paths)-1], []byte(`{"broken":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReconcileBackupStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot); err != nil {
		t.Fatalf("admitted snapshot dropped on trusted reconcile: %v", err)
	}
	// ...but the background scrub re-verifies seals and drops it so it stops
	// pinning objects. Seal checks live at restore and here, not at startup.
	svc.scrub(context.Background(), []string{source})
	svc.wg.Wait()
	if _, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("scrub kept bit-rotted restore point: %v", err)
	}
	if _, err := os.Stat(paths[len(paths)-1]); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("scrub left corrupt manifest pinning objects: %v", err)
	}
}

// Recovery reclaims its source through the same corrupt-tolerant pass as
// startup: a bit-rotted sibling manifest is dropped instead of deferring the
// collection, and the usage cache is refreshed.
func TestPublishedRecoveryDropsCorruptSiblingAndRefreshesUsage(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0007"
	row := pendingCleanupSnapshot(source)
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	corruptID := "dddddddd-0000-4000-8000-000000000004"
	corruptPath := filepath.Join(root, source, "snapshots", corruptID+".json")
	if err := os.WriteFile(corruptPath, []byte(`{"broken":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	transferred := int64(1234)
	recovered, err := svc.reconcileStagingSnapshot(context.Background(), row, &transferred)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatal("published snapshot was not recovered")
	}
	backup, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if backup.TransferredBytes == nil || *backup.TransferredBytes != transferred {
		t.Fatalf("transferred bytes = %v, want %d", backup.TransferredBytes, transferred)
	}
	if _, err := os.Stat(corruptPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("corrupt sibling manifest survived recovery: %v", err)
	}
	devices, err := svc.DeviceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].DiskBytes == nil {
		t.Fatalf("source usage should be refreshed by recovery: %+v", devices)
	}
}

// The run is over whether or not the manifest reads back, so its staging
// envelope must go: a stranded one silently blocks collection for the source.
func TestUnreadableManifestStillClearsStagingEnvelope(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0011"
	row := pendingCleanupSnapshot(source)
	stagingDir, _ := writeUnpublishedStagingState(t, root, source)
	manifestPath := filepath.Join(root, source, "snapshots", cleanupTestSnapshot+".json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(`{"broken":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	recovered, err := svc.reconcileStagingSnapshot(context.Background(), row, nil)
	if err == nil {
		t.Fatal("unreadable manifest was reported as reconciled")
	}
	if recovered {
		t.Fatal("unreadable manifest must not count as a recovered snapshot")
	}
	requireCleanupPathAbsent(t, stagingDir)
}

func TestCatalogReconcilePreservesRuntimeFactsForUnchangedSnapshot(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0008"
	row := pendingCleanupSnapshot(source)
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	transferred := int64(4321)
	if recovered, err := svc.reconcileStagingSnapshot(context.Background(), row, &transferred); err != nil || !recovered {
		t.Fatalf("recover published snapshot: recovered=%v error=%v", recovered, err)
	}
	before, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.reconcileCatalogFromStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if before.StartedAt == nil || after.StartedAt == nil || *after.StartedAt != *before.StartedAt {
		t.Fatalf("start time changed during reconciliation: before=%v after=%v", before.StartedAt, after.StartedAt)
	}
	if after.TransferredBytes == nil || *after.TransferredBytes != transferred {
		t.Fatalf("transfer history changed during reconciliation: %v", after.TransferredBytes)
	}
}

func TestCatalogReconcileMovesStaleSnapshotOwnershipToManifestSource(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const staleSource = "zsourcephone"
	const manifestSource = "asourcephone"
	row := pendingCleanupSnapshot(staleSource)
	stalePaths := writePublishedSnapshot(t, root, staleSource, cleanupTestSnapshot)
	if recovered, err := svc.reconcileStagingSnapshot(context.Background(), row, nil); err != nil || !recovered {
		t.Fatalf("recover stale catalog owner: recovered=%v error=%v", recovered, err)
	}
	if err := os.Remove(stalePaths[len(stalePaths)-1]); err != nil {
		t.Fatal(err)
	}
	writePublishedSnapshot(t, root, manifestSource, cleanupTestSnapshot)

	if _, err := svc.reconcileCatalogFromStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	backup, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if backup.SourceUDID != manifestSource {
		t.Fatalf("catalog source = %q, want manifest source %q", backup.SourceUDID, manifestSource)
	}
	if backup.StartedAt != nil || backup.TransferredBytes != nil {
		t.Fatalf("runtime facts survived ownership change: %+v", backup)
	}
}
