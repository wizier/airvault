package service

import (
	"cmp"
	"context"

	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
)

// RestoreSources lists every restore point from the catalog alone — the cached
// per-snapshot metadata means no manifest is reparsed on a page load.
func (s *Service) RestoreSources(ctx context.Context) ([]RestorePoint, error) {
	snapshots, err := s.store.Backup.ListComplete(ctx)
	if err != nil {
		return nil, err
	}
	devices, err := s.store.Device.List(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(devices))
	for _, device := range devices {
		names[device.UDID] = device.Name
	}
	out := make([]RestorePoint, 0, len(snapshots))
	for i := range snapshots {
		point := restorePoint(&snapshots[i])
		point.UDID = snapshots[i].SourceUDID
		point.DeviceName = cmp.Or(names[point.UDID], point.DeviceName)
		out = append(out, point)
	}
	return out, nil
}

func snapshotProjection(view *objectstore.View, source, snapshotID string) (model.Backup, error) {
	info, err := iosbackup.Inspect(view)
	if err != nil {
		return model.Backup{}, err
	}
	return model.Backup{
		ID: snapshotID, SourceUDID: source,
		SizeBytes: view.SizeBytes(),
		Encrypted: info.Encrypted, IOSVersion: info.IOSVersion,
		DeviceName: info.DeviceName, ProductType: info.ProductType,
		CreatedAt: view.CreatedUnix(),
	}, nil
}
