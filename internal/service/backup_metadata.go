package service

import (
	"context"
	"fmt"
	"io"

	"github.com/wizier/airvault/internal/model"
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

// backupInfo is the manifest-derived metadata cached in SQLite for a snapshot.
type backupInfo struct {
	encrypted   bool
	iosVersion  string
	deviceName  string
	productType string
}

// inspectBackupView validates the minimum iOS backup structure required for a
// restore and returns only metadata that is actually cached in SQLite.
func inspectBackupView(view *objectstore.View) (backupInfo, error) {
	var manifest backupManifest
	if err := readViewPlist(view, "Manifest.plist", &manifest); err != nil {
		return backupInfo{}, fmt.Errorf("read Manifest.plist: %w", err)
	}

	var status backupStatus
	if err := readViewPlist(view, "Status.plist", &status); err != nil {
		return backupInfo{}, fmt.Errorf("read Status.plist: %w", err)
	}
	if status.SnapshotState != "finished" {
		return backupInfo{}, fmt.Errorf("backup Status.plist SnapshotState is %q, want %q",
			status.SnapshotState, "finished")
	}

	var info map[string]any
	if err := readViewPlist(view, "Info.plist", &info); err != nil {
		return backupInfo{}, fmt.Errorf("read Info.plist: %w", err)
	}
	if target, ok := info["Target Identifier"].(string); !ok || target == "" {
		return backupInfo{}, fmt.Errorf("backup Info.plist has no Target Identifier")
	}
	size, ok := view.FileSize("Manifest.db")
	if !ok || size == 0 {
		return backupInfo{}, fmt.Errorf("backup Manifest.db is not a non-empty regular file")
	}
	file, err := view.Open("Manifest.db")
	if err != nil {
		return backupInfo{}, fmt.Errorf("open Manifest.db: %w", err)
	}
	if err := file.Close(); err != nil {
		return backupInfo{}, fmt.Errorf("close Manifest.db: %w", err)
	}
	name, _ := info["Device Name"].(string)
	product, _ := info["Product Type"].(string)
	return backupInfo{
		encrypted:   manifest.IsEncrypted,
		deviceName:  name,
		productType: product,
		iosVersion:  manifest.Lockdown.ProductVersion,
	}, nil
}

// RestoreSources lists every restore point from the catalog alone — the cached
// per-snapshot metadata means no manifest is reparsed on a page load.
func (s *Service) RestoreSources(ctx context.Context) ([]RestorePoint, error) {
	snapshots, err := s.store.Backup.ListComplete(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RestorePoint, 0, len(snapshots))
	for i := range snapshots {
		point := restorePoint(&snapshots[i])
		point.UDID = snapshots[i].SourceUDID
		if device, getErr := s.store.Device.GetByUDID(ctx, point.UDID); getErr == nil && device.Name != "" {
			point.DeviceName = device.Name
		}
		out = append(out, point)
	}
	return out, nil
}

func snapshotProjection(view *objectstore.View, source, snapshotID string) (model.Backup, error) {
	info, err := inspectBackupView(view)
	if err != nil {
		return model.Backup{}, err
	}
	return model.Backup{
		ID: snapshotID, SourceUDID: source,
		SizeBytes: view.SizeBytes(),
		Encrypted: info.encrypted, IOSVersion: info.iosVersion,
		DeviceName: info.deviceName, ProductType: info.productType,
		CreatedAt: view.CreatedUnix(),
	}, nil
}

func readViewPlist(view *objectstore.View, logicalPath string, value any) error {
	file, err := view.Open(logicalPath)
	if err != nil {
		return err
	}
	defer file.Close()
	// OOM guard only — Info.plist grows with the app census and can pass
	// 64 MiB on large libraries, so the ceiling stays far above the real world.
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
