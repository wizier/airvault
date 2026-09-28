package durable

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileReplacesWithoutLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.plist")
	for _, content := range []string{"old", "new"} {
		if err := WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new" {
		t.Fatalf("content = %q, %v", data, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want the file alone", len(entries))
	}
}
