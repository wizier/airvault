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

var brokenManifest = []byte(`{"broken":true}`)

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

func writeUnpublishedStagingState(t *testing.T, root, source string) string {
	t.Helper()
	stagingDir := filepath.Join(root, source, "staging", cleanupTestSnapshot)
	writeTestFile(t, filepath.Join(stagingDir, "manifest.json.tmp"), []byte(`{"incomplete":true}`))
	writeTestFile(t, filepath.Join(stagingDir, "objects", "random.tmp"), []byte("unfinished"))
	return stagingDir
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

func requirePathsPresent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("path %s was removed: %v", path, err)
		}
	}
}

func requireNotCataloged(t *testing.T, svc *Service, snapshotID string) {
	t.Helper()
	if _, err := svc.store.Backup.Get(context.Background(), snapshotID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("snapshot %s is in the catalog: %v", snapshotID, err)
	}
}

func reconcileStore(t *testing.T, svc *Service) []string {
	t.Helper()
	sources, err := svc.ReconcileBackupStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sources
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
	stagingDir := writeUnpublishedStagingState(t, root, source)
	reconcileStore(t, svc)

	fresh, baseSnapshotID, err := svc.prepareSnapshot(context.Background(), &runReservation{udid: source})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == cleanupTestSnapshot || baseSnapshotID != "" {
		t.Fatalf("prepareSnapshot = (%s, base %q), want a fresh snapshot with no base", fresh.ID, baseSnapshotID)
	}
	requireNotCataloged(t, svc, cleanupTestSnapshot)
	requireNotCataloged(t, svc, fresh.ID)
	requireCleanupPathAbsent(t, stagingDir)
}

func TestStartupRecoversPublishedAttemptAsIncrementalBase(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0004"
	publishedPaths := writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	stagingDir := filepath.Join(root, source, "staging", cleanupTestSnapshot)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reconcileStore(t, svc)

	fresh, baseSnapshotID, err := svc.prepareSnapshot(context.Background(), &runReservation{udid: source})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == cleanupTestSnapshot || baseSnapshotID != cleanupTestSnapshot {
		t.Fatalf("prepareSnapshot = (%s, base %q), want a fresh snapshot on base %s", fresh.ID, baseSnapshotID, cleanupTestSnapshot)
	}
	requireCleanupPathAbsent(t, stagingDir)
	recovered, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.CreatedAt == 0 {
		t.Fatalf("recovered snapshot has no creation time: %+v", recovered)
	}
	requirePathsPresent(t, publishedPaths...)
}

// Deleting a device never reads a manifest and never asks the collector what it
// recognises: whatever the source left behind, the tree goes.
func TestDeleteBackupSourceRemovesSourceWhateverItLeftBehind(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0002"
	writeUnpublishedStagingState(t, root, source)
	writeTestFile(t, filepath.Join(root, source, "objects", "not-an-object-prefix"), []byte("junk"))
	writeTestFile(t, filepath.Join(root, source, "snapshots", cleanupTestSnapshot+".json"), brokenManifest)

	lease, err := svc.ops.acquire("test", snapshotWriteResource(source))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.deleteBackupSource(context.Background(), lease, source); err != nil {
		t.Fatal(err)
	}
	// Reclamation runs in the background and owns the write lease.
	svc.wg.Wait()
	requireCleanupPathAbsent(t, filepath.Join(root, source))
	if _, err := svc.ops.acquire("test", snapshotWriteResource(source)); err != nil {
		t.Fatalf("write lease was not released after reclamation: %v", err)
	}
}

func TestDeleteSnapshotsRecountsUsageThenEmptiesTheSource(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0014"
	const keptID = "eeeeeeee-0000-4000-8000-000000000005"
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	keptPaths := writePublishedSnapshot(t, root, source, keptID)
	reconcileStore(t, svc)

	if err := svc.DeleteSnapshots(context.Background(), source, []string{cleanupTestSnapshot}); err != nil {
		t.Fatal(err)
	}
	requireNotCataloged(t, svc, cleanupTestSnapshot)
	kept, err := svc.store.Backup.Get(context.Background(), keptID)
	if err != nil {
		t.Fatal(err)
	}
	manifestInfo, err := os.Stat(keptPaths[len(keptPaths)-1])
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

	// With the last snapshot gone nothing is reachable: collection takes manifest,
	// objects and directories alike, so the source stops being listed.
	if err := svc.DeleteSnapshots(context.Background(), source, []string{keptID}); err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()
	requireCleanupPathAbsent(t, filepath.Join(root, source))
	if sources, err := svc.objects.ListSources(); err != nil || len(sources) != 0 {
		t.Fatalf("emptied source is still listed: %v, %v", sources, err)
	}
	if _, err := svc.ops.acquire("test", snapshotWriteResource(source)); err != nil {
		t.Fatalf("write lease was not released after background collection: %v", err)
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

	// Every restart keeps the same owner, never flipping to the copy.
	for pass := range 2 {
		if sources := reconcileStore(t, svc); len(sources) != 2 {
			t.Fatalf("pass %d: sources = %v, want both", pass, sources)
		}
		row, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
		if err != nil {
			t.Fatal(err)
		}
		if row.SourceUDID != kept {
			t.Fatalf("pass %d: snapshot owner = %s, want %s", pass, row.SourceUDID, kept)
		}
	}
	// The copy stays on disk, so collection keeps its objects reachable.
	requirePathsPresent(t, copied...)
}

// Reconcile rebuilds the catalog from manifests, returns every source for the
// background scrub, keeps a surviving source's footprint across restarts and
// forgets a snapshot whose manifest disappeared.
func TestReconcileKeepsCatalogInStepWithManifests(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0005"
	paths := writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	manifestPath := paths[len(paths)-1]

	if sources := reconcileStore(t, svc); len(sources) != 1 || sources[0] != source {
		t.Fatalf("reconcile sources = %v, want [%s]", sources, source)
	}
	backup, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if backup.SizeBytes <= 0 || backup.CreatedAt != fixtureCreatedUnix || backup.TransferredBytes != nil || backup.StartedAt != nil {
		t.Fatalf("rebuilt backup = %+v, want manifest size and creation time, unknown runtime facts", backup)
	}
	devices, err := svc.DeviceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].UDID != source || devices[0].RestorePoints != 1 {
		t.Fatalf("backup-only devices = %+v", devices)
	}

	// Footprint is background maintenance, not part of catalog reconcile.
	svc.scrub(context.Background(), []string{source})
	svc.wg.Wait()
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	// The footprint counts live objects plus the published manifest itself.
	want := backup.SizeBytes + manifestInfo.Size()
	if got := sourceFootprint(t, svc, source); got == nil || *got != want {
		t.Fatalf("scrubbed footprint = %v, want %d", got, want)
	}

	// A restart re-runs reconcile before the async scrub: an unchanged source is
	// still returned for verification and its footprint is not blanked.
	if sources := reconcileStore(t, svc); len(sources) != 1 || sources[0] != source {
		t.Fatalf("unchanged reconcile sources = %v, want [%s]", sources, source)
	}
	if got := sourceFootprint(t, svc, source); got == nil || *got != want {
		t.Fatalf("reconcile blanked surviving footprint = %v, want %d", got, want)
	}

	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	reconcileStore(t, svc)
	requireNotCataloged(t, svc, cleanupTestSnapshot)
	if devices, err := svc.DeviceList(context.Background()); err != nil || len(devices) != 0 {
		t.Fatalf("backup-only device survived missing manifest: %+v, %v", devices, err)
	}
}

func TestStartupMaintenanceSkipsBusySourceAndCollectsOnNextPass(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0017"
	const abandonedID = "eeeeeeee-0000-4000-8000-000000000005"
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	reconcileStore(t, svc)
	const objectRef = "9999999999999999999999999999999999999999999999999999999999999999"
	staged := filepath.Join(root, source, "staging", abandonedID, "objects", "orphan.tmp")
	writeTestFile(t, staged, []byte("abandoned object"))
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
	sources := reconcileStore(t, svc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A busy source is left to its owner: the one-shot pass skips it without
	// touching the pool and does not retry.
	release, err := svc.ops.acquire("test", snapshotWriteResource(source))
	if err != nil {
		t.Fatal(err)
	}
	svc.StartMaintenance(ctx, sources)
	svc.wg.Wait()
	requirePathsPresent(t, pooled)
	release()

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
	reconcileStore(t, svc)
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
	requirePathsPresent(t, paths[:len(paths)-1]...)
}

// Reconcile opens only manifests new to the catalog: a corrupt newcomer is
// never advertised and is unlinked, while bit rot in an admitted one is left
// for the scrub, which re-verifies seals and drops it so it stops pinning objects.
func TestCorruptManifestIsDroppedAtAdmissionOrByScrub(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0009"
	const newcomerID = "dddddddd-0000-4000-8000-000000000004"
	paths := writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	admittedPath := paths[len(paths)-1]
	reconcileStore(t, svc)

	newcomerPath := filepath.Join(root, source, "snapshots", newcomerID+".json")
	writeTestFile(t, newcomerPath, brokenManifest)
	writeTestFile(t, admittedPath, brokenManifest)
	reconcileStore(t, svc)
	requireNotCataloged(t, svc, newcomerID)
	requireCleanupPathAbsent(t, newcomerPath)
	if _, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot); err != nil {
		t.Fatalf("admitted snapshot dropped on trusted reconcile: %v", err)
	}

	svc.scrub(context.Background(), []string{source})
	svc.wg.Wait()
	requireNotCataloged(t, svc, cleanupTestSnapshot)
	requireCleanupPathAbsent(t, admittedPath)
}

// Recovery reclaims its source through the same corrupt-tolerant pass as
// startup: a bit-rotted sibling manifest is dropped instead of deferring the
// collection, and the usage cache is refreshed. The runtime facts it records
// survive a later catalog reconcile of the unchanged snapshot.
func TestPublishedRecoveryDropsCorruptSiblingAndKeepsRuntimeFacts(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0007"
	row := pendingCleanupSnapshot(source)
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	corruptPath := filepath.Join(root, source, "snapshots", "dddddddd-0000-4000-8000-000000000004.json")
	writeTestFile(t, corruptPath, brokenManifest)

	transferred := int64(1234)
	recovered, err := svc.reconcileStagingSnapshot(context.Background(), row, &transferred)
	if err != nil || !recovered {
		t.Fatalf("recover published snapshot: recovered=%v error=%v", recovered, err)
	}
	requireCleanupPathAbsent(t, corruptPath)
	if got := sourceFootprint(t, svc, source); got == nil {
		t.Fatal("source usage should be refreshed by recovery")
	}

	if _, err := svc.reconcileCatalogFromStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	backup, err := svc.store.Backup.Get(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if backup.StartedAt == nil || *backup.StartedAt != *row.StartedAt {
		t.Fatalf("start time = %v, want %d", backup.StartedAt, *row.StartedAt)
	}
	if backup.TransferredBytes == nil || *backup.TransferredBytes != transferred {
		t.Fatalf("transferred bytes = %v, want %d", backup.TransferredBytes, transferred)
	}
}

// The run is over whether or not the manifest reads back, so its staging
// envelope must go: a stranded one silently blocks collection for the source.
func TestUnreadableManifestStillClearsStagingEnvelope(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0011"
	stagingDir := writeUnpublishedStagingState(t, root, source)
	writeTestFile(t, filepath.Join(root, source, "snapshots", cleanupTestSnapshot+".json"), brokenManifest)

	recovered, err := svc.reconcileStagingSnapshot(context.Background(), pendingCleanupSnapshot(source), nil)
	if err == nil || recovered {
		t.Fatalf("unreadable manifest reconciled: recovered=%v error=%v, want an error", recovered, err)
	}
	requireCleanupPathAbsent(t, stagingDir)
}

func TestCatalogReconcileMovesStaleSnapshotOwnershipToManifestSource(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const staleSource = "zsourcephone"
	const manifestSource = "asourcephone"
	stalePaths := writePublishedSnapshot(t, root, staleSource, cleanupTestSnapshot)
	if recovered, err := svc.reconcileStagingSnapshot(context.Background(), pendingCleanupSnapshot(staleSource), nil); err != nil || !recovered {
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
