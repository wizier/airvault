package service

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/model"
)

type DeviceOverview struct {
	UDID        string `json:"udid"`
	Name        string `json:"name"`
	ProductType string `json:"productType,omitempty"`
	IOSVersion  string `json:"iosVersion,omitempty"`
	Connection  string `json:"connection"` // "wifi" | "usb" | "offline"
	Paired      bool   `json:"paired"`
	Encrypted   bool   `json:"encrypted"`
	LockScreen  bool   `json:"lockScreen,omitempty"` // SpringBoard locked/asleep; selects its wallpaper
	// ActivationState mirrors lockdown for online phones ("Activated",
	// "Unactivated", ...); empty = unknown. Setup Assistant reads Unactivated.
	ActivationState string     `json:"activationState,omitempty"`
	LastSeen        *time.Time `json:"lastSeen,omitempty"`
	LastBackup      *time.Time `json:"lastBackup,omitempty"`
	// Run kind ("backup"/"restore") to the stable error code of its most recent
	// failed run; the client localizes it.
	LastRunErrors map[string]string `json:"lastRunErrors,omitempty"`
	// Live objects plus published manifests; nil means unknown.
	DiskBytes     *int64 `json:"diskBytes,omitempty"`
	RestorePoints int    `json:"restorePoints,omitempty"`
	// AutoBackup is absent for orphaned sources, which have no settings.
	AutoBackup *AutoBackupView `json:"autoBackup,omitempty"`
	// Orphaned marks a source with restore points on disk but no registry row —
	// its phone was removed while the backups stayed.
	Orphaned bool `json:"orphaned,omitempty"`
}

func (s *Service) decorate(d model.Device, rt map[string]deviceRuntime) DeviceOverview {
	r := rt[d.UDID]
	return DeviceOverview{
		UDID: d.UDID, Name: d.Name, ProductType: d.ProductType, IOSVersion: d.IOSVersion,
		Connection: cmp.Or(r.presence, "offline"), Paired: d.Paired, Encrypted: d.Encrypted,
		LastSeen: optionalTime(d.LastSeenAt), LockScreen: r.presence != "" && r.lockScreen(),
		ActivationState: r.activation, LastRunErrors: s.lastRunErrors(d.UDID),
	}
}

// Shared by the per-device list and the global restore-source list, which also
// fills UDID.
type RestorePoint struct {
	UDID             string     `json:"udid,omitempty"`
	SnapshotID       string     `json:"snapshotId"`
	SizeBytes        int64      `json:"sizeBytes"`
	TransferredBytes *int64     `json:"transferredBytes"`
	StartedAt        *time.Time `json:"started,omitempty"`
	CreatedAt        time.Time  `json:"created"`
	Encrypted        bool       `json:"encrypted,omitempty"`
	IOSVersion       string     `json:"iosVersion,omitempty"`
	DeviceName       string     `json:"deviceName,omitempty"`
}

func restorePoint(row *model.Backup) RestorePoint {
	return RestorePoint{
		SnapshotID: row.ID, SizeBytes: row.SizeBytes,
		TransferredBytes: row.TransferredBytes,
		StartedAt:        optionalTime(row.StartedAt), CreatedAt: time.Unix(row.CreatedAt, 0).UTC(),
		Encrypted: row.Encrypted, IOSVersion: row.IOSVersion, DeviceName: row.DeviceName,
	}
}

func optionalTime(unix *int64) *time.Time {
	if unix == nil {
		return nil
	}
	value := time.Unix(*unix, 0).UTC()
	return &value
}

func (s *Service) DeviceList(ctx context.Context) ([]DeviceOverview, error) {
	devices, err := s.store.Device.List(ctx)
	if err != nil {
		return nil, err
	}
	summary, err := s.library.Summary(ctx)
	if err != nil {
		return nil, err
	}
	rt := s.live.snapshot()
	now := time.Now()
	out := make([]DeviceOverview, 0, len(devices)+len(summary))
	seen := make(map[string]struct{}, len(devices))
	for _, d := range devices {
		seen[d.UDID] = struct{}{}
		ov := s.decorate(d, rt)
		if gs, ok := summary[d.UDID]; ok {
			created := time.Unix(gs.LatestCreated, 0).UTC()
			ov.LastBackup = &created
			ov.DiskBytes = gs.DiskBytes
			ov.RestorePoints = gs.RestorePoints
		}
		ov.AutoBackup = s.autoBackupView(d, ov.LastBackup, now)
		out = append(out, ov)
	}
	// SQLite's device registry is disposable. A source rebuilt from manifests
	// must remain visible even when that phone has not been discovered again.
	for source, stored := range summary {
		if _, ok := seen[source]; ok {
			continue
		}
		created := time.Unix(stored.LatestCreated, 0).UTC()
		out = append(out, DeviceOverview{
			UDID: source, Name: cmp.Or(stored.DeviceName, source), Connection: "offline",
			ProductType: stored.ProductType, IOSVersion: stored.IOSVersion, LastBackup: &created,
			DiskBytes: stored.DiskBytes, RestorePoints: stored.RestorePoints, Orphaned: true,
		})
	}
	slices.SortFunc(out, func(a, b DeviceOverview) int { return cmp.Compare(a.UDID, b.UDID) })
	return out, nil
}

func (s *Service) pairedDevice(ctx context.Context, udid string) (*model.Device, error) {
	device, err := s.store.Device.GetByUDID(ctx, udid)
	if err != nil {
		return nil, err
	}
	if !device.Paired {
		return nil, domain.ErrPairingRequired
	}
	return device, nil
}

func (s *Service) RestorePoints(ctx context.Context, udid string) ([]RestorePoint, error) {
	rows, err := s.library.RestorePoints(ctx, udid)
	if err != nil {
		return nil, err
	}
	points := make([]RestorePoint, 0, len(rows))
	for i := range rows {
		points = append(points, restorePoint(&rows[i]))
	}
	return points, nil
}

// Catalog only: no manifest is reparsed on a page load.
func (s *Service) RestoreSources(ctx context.Context) ([]RestorePoint, error) {
	snapshots, err := s.library.AllRestorePoints(ctx)
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

// Only Unpair calls this, holding the device write lease; otherwise discovery
// would re-register a reachable device seconds later.
func (s *Service) forgetDevice(ctx context.Context, udid string) error {
	// removeLocal emits no offline transition, so stop the observer directly.
	s.lockObs.setOffline(udid)
	s.live.removeLocal(udid)
	s.clearRunOutcomes(udid)
	s.gallery.remove(udid)
	if err := s.store.Device.Delete(ctx, udid); err != nil {
		return err
	}
	s.bus.Emit(deviceRemoved(udid))
	return nil
}

// An unreachable phone never blocks the removal; only a failed host-side
// cleanup is an error.
func (s *Service) Unpair(ctx context.Context, udid string, deleteBackups bool) error {
	if _, err := s.store.Device.GetByUDID(ctx, udid); err != nil {
		return err
	}
	release, err := s.acquireFor(ctx, "unpair", udid, deviceWriteResource(udid))
	if err != nil {
		return err
	}
	defer release()
	// Taken up front so a busy source fails the request before the phone is
	// touched; the removal behind the deletion inherits it.
	var releaseSnapshots func()
	handedOff := false
	if deleteBackups {
		if releaseSnapshots, err = s.acquireFor(ctx, "unpair", udid, snapshotWriteResource(udid)); err != nil {
			return err
		}
		// Every path that does not reach DeleteSource gives the lease back
		// here; a leaked one wedges the source until restart.
		defer func() {
			if !handedOff {
				releaseSnapshots()
			}
		}()
	}
	// A discovery pass reads several values before its eventual Upsert. Keep an
	// old paired snapshot from being committed after this revoke+delete.
	s.deviceRefreshMu.Lock()
	defer s.deviceRefreshMu.Unlock()
	if err := s.engine.Unpair(ctx, engine.DeviceID(udid)); err != nil {
		slog.Warn("unpair failed", "udid", udid, "error", err)
		return fmt.Errorf("%w: %v", domain.ErrPairingCleanup, err)
	}
	// Host-side pairing is already gone; finish even if the browser disconnects.
	finalCtx := context.WithoutCancel(ctx)
	if deleteBackups {
		handedOff = true
		if err := s.library.DeleteSource(finalCtx, udid, releaseSnapshots); err != nil {
			return err
		}
	}
	if err := s.forgetDevice(finalCtx, udid); err != nil {
		return err
	}
	// Emit only after the registry write committed, so a refetch it triggers
	// can't see stale rows.
	s.bus.Emit(pairingChanged(udid, false))
	return nil
}

func (s *Service) reachableDevice(ctx context.Context, udid string) error {
	if _, err := s.pairedDevice(ctx, udid); err != nil {
		return err
	}
	if s.live.connection(udid) == "" {
		return domain.ErrDeviceOffline
	}
	return nil
}
