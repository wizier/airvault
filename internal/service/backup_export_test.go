package service

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/domain"
)

// An export is named after the phone and packs a "<UDID>-<date>" folder that
// can't merge into an existing backup of the same phone.
func TestOpenBackupExportNamesFileAndFolder(t *testing.T) {
	svc, root := newSnapshotCleanupService(t)
	const source = "testphoneudid0007"
	writePublishedSnapshot(t, root, source, cleanupTestSnapshot)
	reconcileStore(t, svc)

	export, err := svc.OpenBackupExport(context.Background(), cleanupTestSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Unix(fixtureCreatedUnix, 0)
	if want := "AirVault-Test iPhone-" + created.Format("2006-01-02") + ".tar"; export.Name != want {
		t.Fatalf("name = %q, want %q", export.Name, want)
	}
	var out bytes.Buffer
	if _, err := export.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	header, err := tar.NewReader(&out).Next()
	if err != nil {
		t.Fatal(err)
	}
	if want := source + "-" + created.Format("20060102-150405") + "/"; header.Name != want {
		t.Fatalf("root folder = %q, want %q", header.Name, want)
	}
}

func TestOpenBackupExportUnknownSnapshotIsNotFound(t *testing.T) {
	svc, _ := newSnapshotCleanupService(t)
	if _, err := svc.OpenBackupExport(context.Background(), cleanupTestSnapshot); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}
