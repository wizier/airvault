package handler

import (
	"encoding/json"
	"testing"

	"github.com/wizier/airvault/internal/service"
)

func TestDefaultRestoreOptionsSurviveOmittedJSONFields(t *testing.T) {
	opts := restoreRequest(service.DefaultRestoreOptions())
	if err := json.Unmarshal([]byte(`{"snapshotId":"snapshot-1"}`), &opts); err != nil {
		t.Fatal(err)
	}
	if opts.SnapshotID != "snapshot-1" {
		t.Fatalf("snapshot ID = %q, want snapshot-1", opts.SnapshotID)
	}
	if !opts.SystemFiles || !opts.Reboot || !opts.SettingsFromBackup || !opts.RemoveItemsNotRestored {
		t.Fatalf("omitted restore options did not keep defaults: %#v", opts)
	}
}

func TestDefaultRestoreOptionsAllowExplicitOverrides(t *testing.T) {
	opts := restoreRequest(service.DefaultRestoreOptions())
	body := []byte(`{
		"snapshotId":"snapshot-1",
		"systemFiles":false,
		"reboot":false,
		"settingsFromBackup":false,
		"removeItemsNotRestored":false
	}`)
	if err := json.Unmarshal(body, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.SystemFiles || opts.Reboot || opts.SettingsFromBackup || opts.RemoveItemsNotRestored {
		t.Fatalf("explicit restore overrides were ignored: %#v", opts)
	}
}
