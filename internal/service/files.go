package service

import (
	"context"
	"log/slog"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/domain"
)

func parseRequiredPath(raw string) (devicefs.Path, error) {
	devicePath, err := devicefs.ParsePath(raw)
	if err != nil || devicePath.String() == "" {
		return devicefs.Path{}, &domain.ValidationError{Code: "invalid_path", Message: "path is required and must be valid"}
	}
	return devicePath, nil
}

func appDocumentsRoot(bundleID string) (devicefs.Root, error) {
	if bundleID == "" {
		return devicefs.Root{}, &domain.ValidationError{Code: "bundle_id_required", Message: "bundle id is required"}
	}
	root, err := devicefs.AppDocuments(bundleID)
	if err != nil {
		return devicefs.Root{}, &domain.ValidationError{Code: "invalid_bundle_id", Message: err.Error()}
	}
	return root, nil
}

func (s *Service) openLeasedSession(ctx context.Context, udid string, root devicefs.Root,
	resource resourceRequest, errorCode string) (*devicefs.Session, func(), error) {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return nil, nil, err
	}
	// Direct acquire: browsing answers the user at once and retries on a click,
	// so a refusal needs no operation event of its own.
	release, err := s.ops.acquire("file access", resource)
	if err != nil {
		return nil, nil, err
	}
	session, err := s.files.Open(ctx, udid, root)
	if err != nil {
		release()
		return nil, nil, newEngineActionError(errorCode, err)
	}
	return session, func() { _ = session.Close(); release() }, nil
}

type FileEntry struct {
	Name     string             `json:"name"`
	Kind     devicefs.EntryKind `json:"kind"`
	Size     *int64             `json:"size,omitempty"`
	Modified *int64             `json:"modified,omitempty"`
	Missing  bool               `json:"missing,omitempty"` // backup only: listed, but its content is not held
}

// Only apps with file sharing enabled expose their Documents (house_arrest).
func (s *Service) AppFiles(ctx context.Context, udid, bundleID, rawPath string) ([]FileEntry, error) {
	root, err := appDocumentsRoot(bundleID)
	if err != nil {
		return nil, err
	}
	return s.deviceFileList(ctx, udid, root, rawPath, "app_files_failed")
}

func (s *Service) deviceFileList(
	ctx context.Context,
	udid string,
	root devicefs.Root,
	rawPath, errorCode string,
) ([]FileEntry, error) {
	devicePath, err := devicefs.ParsePath(rawPath)
	if err != nil {
		return nil, &domain.ValidationError{Code: "invalid_path", Message: err.Error()}
	}
	session, release, err := s.openLeasedSession(ctx, udid, root, deviceReadResource(udid), errorCode)
	if err != nil {
		return nil, err
	}
	defer release()
	entries, err := session.List(devicePath)
	if err != nil {
		slog.WarnContext(ctx, "device files: engine", "udid", udid, "path", rawPath, "error", err)
		return nil, newEngineActionError(errorCode, err)
	}
	// Never nil, so an empty directory serializes as [].
	out := make([]FileEntry, len(entries))
	for i, entry := range entries {
		out[i] = FileEntry{Name: entry.Name, Kind: entry.Kind, Size: entry.Size, Modified: entry.Modified}
	}
	return out, nil
}

func (s *Service) AppFileDelete(ctx context.Context, udid, bundleID, devicePath string) error {
	root, err := appDocumentsRoot(bundleID)
	if err != nil {
		return err
	}
	parsedPath, err := parseRequiredPath(devicePath)
	if err != nil {
		return err
	}
	session, release, err := s.openLeasedSession(ctx, udid, root, deviceWriteResource(udid), "app_file_delete_failed")
	if err != nil {
		return err
	}
	defer release()
	if err := session.Remove(parsedPath); err != nil {
		slog.WarnContext(ctx, "app file delete: engine", "udid", udid, "bundle", bundleID, "path", devicePath, "error", err)
		return newEngineActionError("app_file_delete_failed", err)
	}
	return nil
}

func (s *Service) MediaList(ctx context.Context, udid, rawPath string) ([]FileEntry, error) {
	return s.deviceFileList(ctx, udid, devicefs.Media(), rawPath, "media_list_failed")
}

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
