package service

import (
	"context"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/domain"
)

type DeviceDownload struct {
	*devicefs.File
	cleanup func()
}

func (d *DeviceDownload) Close() {
	d.cleanup()
}

func (s *Service) openDeviceDownload(ctx context.Context, udid string, root devicefs.Root, rawPath string) (*DeviceDownload, error) {
	devicePath, err := parseRequiredPath(rawPath)
	if err != nil {
		return nil, err
	}
	session, release, err := s.openLeasedSession(ctx, udid, root, deviceReadResource(udid), "download_failed")
	if err != nil {
		return nil, err
	}
	// OpenFile hands the AFC session over to the file, so release only frees
	// the lease once the download closes.
	file, err := session.OpenFile(devicePath)
	if err != nil {
		release()
		return nil, newEngineActionError("download_failed", err)
	}
	return &DeviceDownload{
		File:    file,
		cleanup: func() { _ = file.Close(); release() }, // both are idempotent
	}, nil
}

func (s *Service) OpenAppFileDownload(ctx context.Context, udid, bundleID, devicePath string) (*DeviceDownload, error) {
	root, err := appDocumentsRoot(bundleID)
	if err != nil {
		return nil, err
	}
	return s.openDeviceDownload(ctx, udid, root, devicePath)
}

func (s *Service) OpenMediaDownload(ctx context.Context, udid, devicePath string) (*DeviceDownload, error) {
	return s.openDeviceDownload(ctx, udid, devicefs.Media(), devicePath)
}

type DeviceFileStat struct {
	Size     int64  `json:"size"`
	Modified *int64 `json:"modified,omitempty"`
}

func (s *Service) deviceFileStat(ctx context.Context, udid string, root devicefs.Root, rawPath string) (DeviceFileStat, error) {
	devicePath, err := parseRequiredPath(rawPath)
	if err != nil {
		return DeviceFileStat{}, err
	}
	session, release, err := s.openLeasedSession(ctx, udid, root, deviceReadResource(udid), "stat_failed")
	if err != nil {
		return DeviceFileStat{}, err
	}
	defer release()
	entry, err := session.Stat(devicePath)
	if err != nil {
		return DeviceFileStat{}, newEngineActionError("stat_failed", err)
	}
	if entry.Kind != devicefs.EntryFile {
		return DeviceFileStat{}, &domain.ValidationError{Code: "file_required", Message: "path must identify a file"}
	}
	// Stat always sizes a file entry.
	return DeviceFileStat{Size: *entry.Size, Modified: entry.Modified}, nil
}

func (s *Service) AppFileStat(ctx context.Context, udid, bundleID, devicePath string) (DeviceFileStat, error) {
	root, err := appDocumentsRoot(bundleID)
	if err != nil {
		return DeviceFileStat{}, err
	}
	return s.deviceFileStat(ctx, udid, root, devicePath)
}

func (s *Service) MediaStat(ctx context.Context, udid, devicePath string) (DeviceFileStat, error) {
	return s.deviceFileStat(ctx, udid, devicefs.Media(), devicePath)
}
