package iosbackup

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/wizier/airvault/internal/objectstore"
	"howett.net/plist"
)

type backupManifest struct {
	IsEncrypted bool `plist:"IsEncrypted"`
	Lockdown    struct {
		ProductVersion string `plist:"ProductVersion"`
	} `plist:"Lockdown"`
}

type backupStatus struct {
	SnapshotState string `plist:"SnapshotState"`
}

type Info struct {
	Encrypted   bool
	IOSVersion  string
	DeviceName  string
	ProductType string
}

// Inspect checks only the minimum structure a restore needs.
func Inspect(view *objectstore.View) (Info, error) {
	var manifest backupManifest
	if err := readPlist(view, "Manifest.plist", &manifest); err != nil {
		return Info{}, fmt.Errorf("read Manifest.plist: %w", err)
	}

	var status backupStatus
	if err := readPlist(view, "Status.plist", &status); err != nil {
		return Info{}, fmt.Errorf("read Status.plist: %w", err)
	}
	if status.SnapshotState != "finished" {
		return Info{}, fmt.Errorf("backup Status.plist SnapshotState is %q, want %q",
			status.SnapshotState, "finished")
	}

	var info map[string]any
	if err := readPlist(view, "Info.plist", &info); err != nil {
		return Info{}, fmt.Errorf("read Info.plist: %w", err)
	}
	if target, ok := info["Target Identifier"].(string); !ok || target == "" {
		return Info{}, fmt.Errorf("backup Info.plist has no Target Identifier")
	}
	size, ok := view.FileSize("Manifest.db")
	if !ok || size == 0 {
		return Info{}, fmt.Errorf("backup Manifest.db is not a non-empty regular file")
	}
	file, err := view.Open("Manifest.db")
	if err != nil {
		return Info{}, fmt.Errorf("open Manifest.db: %w", err)
	}
	if err := file.Close(); err != nil {
		return Info{}, fmt.Errorf("close Manifest.db: %w", err)
	}
	name, _ := info["Device Name"].(string)
	product, _ := info["Product Type"].(string)
	return Info{
		Encrypted:   manifest.IsEncrypted,
		DeviceName:  name,
		ProductType: product,
		IOSVersion:  manifest.Lockdown.ProductVersion,
	}, nil
}

func readPlist(view *objectstore.View, logicalPath string, value any) error {
	file, err := view.Open(logicalPath)
	if err != nil {
		return err
	}
	defer file.Close()
	// OOM guard only: Info.plist grows with the app census and can pass 64 MiB
	// on large libraries.
	const maxBackupPlistBytes = 256 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxBackupPlistBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxBackupPlistBytes {
		return fmt.Errorf("plist exceeds %d-byte limit", maxBackupPlistBytes)
	}
	_, err = plist.Unmarshal(data, value)
	return err
}

// NewerVersion reports iOS version a strictly newer than b; unknown versions
// never block.
func NewerVersion(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := 0, 0
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}
