package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A busy source is left to its owner: the startup pass skips it without
// touching its objects, and the next pass collects it.
func TestMaintenanceLeavesABusySourceToItsOwner(t *testing.T) {
	svc, root := newStoredService(t)
	const source = "testphoneudid0017"
	orphan := filepath.Join(root, source, "objects", "99", strings.Repeat("9", 64))
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("unreferenced"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	release, err := svc.ops.acquire("backup", snapshotWriteResource(source))
	if err != nil {
		t.Fatal(err)
	}
	svc.StartMaintenance(ctx, []string{source})
	svc.wg.Wait()
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("a busy source was collected: %v", err)
	}
	release()

	svc.StartMaintenance(ctx, []string{source})
	svc.wg.Wait()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("the free source was not collected: %v", err)
	}
}
