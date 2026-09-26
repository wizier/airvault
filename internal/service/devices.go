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
	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/model"
)

// DeviceOverview is the device read model served to transports, JSON shape
// included — SQLite rows and runtime evidence fold into it here.
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
	// LastRunErrors maps run kind ("backup"/"restore") to the stable error code
	// of that kind's most recent failed run; the client localizes it and
	// derives the displayed status from raw facts.
	LastRunErrors map[string]string `json:"lastRunErrors,omitempty"`
	// DiskBytes is the cached on-disk footprint (live objects plus published
	// manifests). Nil means the cache is currently unknown.
	DiskBytes     *int64 `json:"diskBytes,omitempty"`
	RestorePoints int    `json:"restorePoints,omitempty"`
	// Orphaned marks a source with restore points on disk but no registry row —
	// its phone was removed while the backups stayed.
	Orphaned bool `json:"orphaned,omitempty"`
}

func (s *Service) decorate(d model.Device, rt map[string]deviceRuntime) DeviceOverview {
	r := rt[d.UDID]
	return DeviceOverview{
		UDID: d.UDID, Name: d.Name, ProductType: d.ProductType, IOSVersion: d.IOSVersion,
		Connection: cmp.Or(r.presence, "offline"), Paired: d.Paired, Encrypted: d.Encrypted,
		LastSeen: optionalTime(d.LastSeenAt), LockScreen: r.presence != "" && r.screen != screenUnlocked,
		ActivationState: r.activation, LastRunErrors: s.lastRunErrors(d.UDID),
	}
}

// RestorePoint is the single restore-point read model: the per-device list
// omits the empty device-identity fields the global restore-source list fills.
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

// DeviceList returns every registered device with its runtime state.
func (s *Service) DeviceList(ctx context.Context) ([]DeviceOverview, error) {
	devices, err := s.store.Device.List(ctx)
	if err != nil {
		return nil, err
	}
	summary, err := s.store.Backup.SummaryBySource(ctx)
	if err != nil {
		return nil, err
	}
	rt := s.live.snapshot()
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

// RestorePoints returns the device's live restore points, newest first.
func (s *Service) RestorePoints(ctx context.Context, udid string) ([]RestorePoint, error) {
	rows, err := s.store.Backup.ListRestorePoints(ctx, udid)
	if err != nil {
		return nil, err
	}
	points := make([]RestorePoint, 0, len(rows))
	for i := range rows {
		points = append(points, restorePoint(&rows[i]))
	}
	return points, nil
}

// forgetDevice removes a device from the registry. Only Unpair calls it, holding
// the device write lease — discovery would re-register a reachable device
// seconds later.
func (s *Service) forgetDevice(ctx context.Context, udid string) error {
	// removeLocal emits no offline transition, so stop the observer directly.
	s.lockObs.setOffline(udid)
	s.live.removeLocal(udid)
	s.clearLastRunErrors(udid)
	s.gallery.remove(udid)
	if err := s.store.Device.Delete(ctx, udid); err != nil {
		return err
	}
	s.bus.Emit(events.DeviceRemoved, map[string]any{"udid": udid})
	return nil
}

// Unpair asks the phone to forget this host, then removes the host record and
// the registry row (backup files optionally). An unreachable phone never blocks
// the removal; only a failed host-side cleanup is an error.
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
		// Every path that does not reach deleteBackupSource gives the lease back
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
		if err := s.deleteBackupSource(finalCtx, releaseSnapshots, udid); err != nil {
			return err
		}
	}
	if err := s.forgetDevice(finalCtx, udid); err != nil {
		return err
	}
	// Emit only after the registry write committed (write-before-emit, like
	// every other event here), so a refetch it triggers can't see stale rows.
	s.bus.Emit(events.PairChanged, map[string]any{"udid": udid, "paired": false})
	return nil
}

// reachableDevice requires a paired device currently listed by the muxer.
func (s *Service) reachableDevice(ctx context.Context, udid string) error {
	if _, err := s.pairedDevice(ctx, udid); err != nil {
		return err
	}
	if s.live.connection(udid) == "" {
		return domain.ErrDeviceOffline
	}
	return nil
}

// DeleteBackups removes a source's restore points. The device registry may be
// absent because it is rebuilt independently from the on-disk backup catalog.
// No device lease: nothing here touches the phone, source write covers the rest.
func (s *Service) DeleteBackups(ctx context.Context, udid string) error {
	release, err := s.acquireFor(ctx, "backup deletion", udid, snapshotWriteResource(udid))
	if err != nil {
		return err
	}
	if err := s.deleteBackupSource(ctx, release, udid); err != nil {
		return err
	}
	// Wiping a source's history resets its status to "never", stale failure included.
	s.clearLastRunErrors(udid)
	return nil
}
