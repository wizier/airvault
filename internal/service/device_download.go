package service

import (
	"context"
	"io"
	"sync"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/events"
)

// DeviceDownload owns an open phone file and every lease held by its stream.
type DeviceDownload struct {
	file     *devicefs.File
	progress func(done, total int64)
	cleanup  func()
}

func (d *DeviceDownload) Size() int64 { return d.file.Size() }

func (d *DeviceDownload) CopyTo(ctx context.Context, destination io.Writer) error {
	return d.file.CopyTo(ctx, destination, d.progress)
}

func (d *DeviceDownload) Close() {
	d.cleanup()
}

func (s *Service) downloadProgress(downloadID string) func(done, total int64) {
	if downloadID == "" {
		return nil
	}
	lastPercent := -1
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		percent := int(float64(done) * 100 / float64(total))
		if percent > 100 {
			percent = 100
		}
		if percent == lastPercent {
			return
		}
		lastPercent = percent
		s.bus.Emit(events.DownloadProgress, map[string]any{
			"downloadId": downloadID,
			"percent":    percent,
		})
	}
}

func (s *Service) openDeviceDownload(
	ctx context.Context,
	udid string,
	root devicefs.Root,
	rawPath string,
	progressID string,
) (*DeviceDownload, error) {
	devicePath, err := parseRequiredPath(rawPath)
	if err != nil {
		return nil, err
	}
	session, release, err := s.openLeasedSession(ctx, udid, root, resourceRead, "download_failed")
	if err != nil {
		return nil, err
	}
	// OpenFile hands the native session over to the file, so release only frees
	// the lease once the download closes.
	file, err := session.OpenFile(devicePath)
	if err != nil {
		release()
		return nil, newEngineActionError("download_failed", err)
	}
	return &DeviceDownload{
		file:     file,
		progress: s.downloadProgress(progressID),
		cleanup: sync.OnceFunc(func() {
			_ = file.Close()
			release()
		}),
	}, nil
}

func (s *Service) OpenAppFileDownload(
	ctx context.Context,
	udid, bundleID, devicePath, downloadID string,
) (*DeviceDownload, error) {
	root, err := appDocumentsRoot(bundleID)
	if err != nil {
		return nil, err
	}
	return s.openDeviceDownload(ctx, udid, root, devicePath, downloadID)
}

func (s *Service) OpenMediaDownload(
	ctx context.Context,
	udid, devicePath, downloadID string,
) (*DeviceDownload, error) {
	return s.openDeviceDownload(ctx, udid, devicefs.Media(), devicePath, downloadID)
}
