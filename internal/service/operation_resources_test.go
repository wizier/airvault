package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/wizier/airvault/internal/domain"
)

func TestOperationCoordinatorSharesReadsAndExcludesWriter(t *testing.T) {
	coordinator := newOperationManager()
	read := resourceRequest{key: "device:test", mode: resourceRead}
	write := resourceRequest{key: "device:test", mode: resourceWrite}
	first, err := coordinator.acquire("browse", read)
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.acquire("browse", read)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.acquire("restore", write); !errors.Is(err, domain.ErrBusy) {
		t.Fatalf("writer with active readers returned %v, want ErrBusy", err)
	}
	first.Release()
	first.Release()
	if _, err := coordinator.acquire("restore", write); !errors.Is(err, domain.ErrBusy) {
		t.Fatalf("duplicate release dropped another reader: %v", err)
	}
	second.Release()
	writer, err := coordinator.acquire("restore", write)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release()
	if _, err := coordinator.acquire("browse", read); !errors.Is(err, domain.ErrBusy) {
		t.Fatalf("reader with active writer returned %v, want ErrBusy", err)
	}
}

func TestOperationCoordinatorAcquiresMultipleResourcesAtomically(t *testing.T) {
	coordinator := newOperationManager()
	busy, err := coordinator.acquire("maintenance", resourceRequest{key: "snapshot:b", mode: resourceWrite})
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Release()
	_, err = coordinator.acquire("backup",
		resourceRequest{key: "snapshot:a", mode: resourceWrite},
		resourceRequest{key: "snapshot:b", mode: resourceRead},
	)
	if !errors.Is(err, domain.ErrBusy) {
		t.Fatalf("multi-resource acquire returned %v, want ErrBusy", err)
	}
	// A failed atomic admission must not leave snapshot:a leased.
	lease, err := coordinator.acquire("backup", resourceRequest{key: "snapshot:a", mode: resourceWrite})
	if err != nil {
		t.Fatalf("failed admission leaked its first resource: %v", err)
	}
	lease.Release()
}

// A rejection has to explain itself for the log while still reducing to the
// stable code the HTTP edge publishes.
func TestBusyRejectionExplainsItselfAndStaysErrBusy(t *testing.T) {
	coordinator := newOperationManager()
	const udid = "testphoneudid0001"
	held, err := coordinator.acquire("maintenance", snapshotWriteResource(udid))
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.acquire("backup", snapshotWriteResource(udid))
	if !errors.Is(err, domain.ErrBusy) {
		t.Fatalf("rejection = %v, want ErrBusy", err)
	}
	if !strings.Contains(err.Error(), "maintenance") {
		t.Fatalf("rejection %q does not name the holding writer", err)
	}
	held.Release()

	// Browsing and a backup share the device as readers. The browse ends, so
	// naming the reader that created the state would blame an operation that is
	// no longer there — readers are counted instead.
	browse, err := coordinator.acquire("file access", deviceReadResource(udid))
	if err != nil {
		t.Fatal(err)
	}
	backup, err := coordinator.acquire("backup", deviceReadResource(udid))
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Release()
	browse.Release()

	_, err = coordinator.acquire("restore", deviceWriteResource(udid))
	if !errors.Is(err, domain.ErrBusy) {
		t.Fatalf("rejection = %v, want ErrBusy", err)
	}
	if !strings.Contains(err.Error(), "1 reader") {
		t.Fatalf("rejection %q does not count the readers", err)
	}
	if strings.Contains(err.Error(), "file access") {
		t.Fatalf("rejection %q names a reader that already left", err)
	}
}
