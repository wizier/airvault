package service

import (
	"context"
	"io"
	"sync"

	"github.com/wizier/airvault/internal/devicefs"
)

// DeviceDownload owns an open phone file and every lease held by its stream.
type DeviceDownload struct {
	file    *devicefs.File
	cleanup func()
}

func (d *DeviceDownload) Size() int64 { return d.file.Size() }

func (d *DeviceDownload) CopyTo(ctx context.Context, destination io.Writer) error {
	return d.file.CopyTo(ctx, destination)
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
	// OpenFile hands the native session over to the file, so release only frees
	// the lease once the download closes.
	file, err := session.OpenFile(devicePath)
	if err != nil {
		release()
		return nil, newEngineActionError("download_failed", err)
	}
	return &DeviceDownload{
		file: file,
		cleanup: sync.OnceFunc(func() {
			_ = file.Close()
			release()
		}),
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
