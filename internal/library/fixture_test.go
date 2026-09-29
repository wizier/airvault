package library

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
	"github.com/wizier/airvault/internal/storage"
)

const testSnapshot = "cccccccc-0000-4000-8000-000000000003"

var brokenManifest = []byte(`{"broken":true}`)

// newTestLibrary is a library over a fresh catalog and object store; it
// returns the store's root.
func newTestLibrary(t *testing.T) (*Library, string) {
	t.Helper()
	catalog, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	objects, err := objectstore.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = objects.Close()
		_ = catalog.Close()
	})
	return New(objects, catalog, func(string) {}), root
}

// fixtureFiles is the minimum an iOS backup must hold to pass validation.
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

// sealFiles writes files into a new snapshot and seals it, as a backup does.
func sealFiles(t *testing.T, lib *Library, source, id string, files map[string]string) *objectstore.StagingView {
	t.Helper()
	session, err := lib.Begin(source, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		writer, err := session.Create(name)
		if err == nil {
			_, err = io.WriteString(writer, body)
		}
		if err == nil {
			err = writer.Commit()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	staged, err := session.Seal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return staged
}

// publishFixture publishes fixtureFiles as a snapshot, uncataloged, and
// returns the source's objects followed by the snapshot's manifest.
func publishFixture(t *testing.T, lib *Library, root, source, id string) []string {
	t.Helper()
	return publishFiles(t, lib, root, source, id, fixtureFiles)
}

func publishFiles(t *testing.T, lib *Library, root, source, id string, files map[string]string) []string {
	t.Helper()
	if _, err := lib.objects.Publish(sealFiles(t, lib, source, id, files)); err != nil {
		t.Fatal(err)
	}
	objects, err := filepath.Glob(filepath.Join(root, source, "objects", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return append(objects, filepath.Join(root, source, "snapshots", id+".json"))
}

// writeUnpublishedStaging leaves the staging a backup cut short leaves.
func writeUnpublishedStaging(t *testing.T, root, source string) string {
	t.Helper()
	stagingDir := filepath.Join(root, source, "staging", testSnapshot)
	writeTestFile(t, filepath.Join(stagingDir, "manifest.json.tmp"), []byte(`{"incomplete":true}`))
	writeTestFile(t, filepath.Join(stagingDir, "objects", "random.tmp"), []byte("unfinished"))
	return stagingDir
}

// pendingRow is what a backup run knows of testSnapshot before publication.
func pendingRow(source string) model.Backup {
	startedAt := int64(1_699_999_000)
	return model.Backup{ID: testSnapshot, SourceUDID: source, StartedAt: &startedAt}
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

func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("path %s still exists or could not be inspected: %v", path, err)
	}
}

func requirePresent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("path %s was removed: %v", path, err)
		}
	}
}

func requireNotCataloged(t *testing.T, lib *Library, id string) {
	t.Helper()
	if _, err := lib.catalog.Backup.Get(context.Background(), id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("snapshot %s is in the catalog: %v", id, err)
	}
}

func reconcile(t *testing.T, lib *Library) []string {
	t.Helper()
	sources, err := lib.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sources
}

func sourceSummary(t *testing.T, lib *Library) map[string]storage.SourceSummary {
	t.Helper()
	summary, err := lib.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

// sourceFootprint reads the size the dashboard reads; nil means unknown.
func sourceFootprint(t *testing.T, lib *Library, source string) *int64 {
	t.Helper()
	return sourceSummary(t, lib)[source].DiskBytes
}

// incompleteFiles is fixtureFiles as a backup the device never finished.
func incompleteFiles() map[string]string {
	files := maps.Clone(fixtureFiles)
	files["Status.plist"] = `<plist version="1.0"><dict><key>SnapshotState</key><string>uploading</string></dict></plist>`
	return files
}
