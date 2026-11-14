package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
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

// A browse that finds a backup damaged leaves the record to whoever holds the
// source: its own collection records it, and a rescan beside it could store a
// stale one.
func TestUnlockLeavesDamageToTheSourcesHolder(t *testing.T) {
	svc, root := newStoredService(t)
	const source, id = "testphoneudid0031", "cccccccc-0000-4000-8000-000000000003"
	ctx := t.Context()
	if err := svc.store.Backup.InsertSnapshot(ctx, model.Backup{ID: id, SourceUDID: source, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, source, "snapshots", id+".json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"broken":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	unlockDamage := func() string {
		t.Helper()
		if err := svc.UnlockBackup(ctx, id, ""); err == nil {
			t.Fatal("a backup with a broken manifest unlocked")
		}
		row, err := svc.store.Backup.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return row.Damage
	}

	release, err := svc.ops.acquire("backup", snapshotWriteResource(source))
	if err != nil {
		t.Fatal(err)
	}
	if damage := unlockDamage(); damage != "" {
		t.Fatalf("a browse recorded %q for a source a backup holds", damage)
	}
	release()
	if damage := unlockDamage(); damage != objectstore.DamageManifestUnreadable {
		t.Fatalf("damage = %q, want the broken manifest recorded", damage)
	}
}
