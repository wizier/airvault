package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
)

// Power sends a restart/shutdown/sleep command. Refused mid-backup (ErrBusy);
// unreachable surfaces as ErrDeviceOffline. No event is emitted — the watcher
// reports the resulting detach/reattach as ordinary presence changes.
func (s *Service) Power(ctx context.Context, udid, action string) error {
	var engineAction engine.PowerAction
	switch action {
	case "restart":
		engineAction = engine.PowerRestart
	case "shutdown":
		engineAction = engine.PowerShutdown
	case "sleep":
		engineAction = engine.PowerSleep
	default:
		return &domain.ValidationError{Code: "invalid_power_action", Message: "action must be restart, shutdown or sleep"}
	}
	if err := s.reachableDevice(ctx, udid); err != nil {
		return err
	}
	err := s.runCommand(ctx, runKindPower, udid, func(ctx context.Context) error {
		if err := s.engine.Power(ctx, engine.DeviceID(udid), engineAction); err != nil {
			slog.DebugContext(ctx, "power: request failed", "udid", udid, "action", action, "error", err)
			return fmt.Errorf("power %s: %w", action, domain.ErrDeviceOffline)
		}
		slog.DebugContext(ctx, "power: accepted", "udid", udid, "action", action)
		return nil
	}, deviceWriteResource(udid))
	return err
}

// LiveBattery is passive telemetry: an on-demand charge read that never wakes
// the phone and never drives presence. Unreachable surfaces as device-offline.
func (s *Service) LiveBattery(ctx context.Context, udid string) (engine.Battery, error) {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return engine.Battery{}, err
	}
	battery, err := s.engine.Battery(ctx, engine.DeviceID(udid))
	if err != nil {
		if errors.Is(err, engine.ErrDeviceUnreachable) {
			return engine.Battery{}, domain.ErrDeviceOffline
		}
		return engine.Battery{}, newEngineActionError("battery_query_failed", err)
	}
	return battery, nil
}

// App is one installed application. FileSharing is true when the app exposes
// its Documents over house_arrest (drives the "Files" action). iOS does not
// report per-app disk size over installation_proxy.
type App struct {
	BundleID    string `json:"bundleId"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	FileSharing bool   `json:"fileSharing,omitempty"`
}

// Apps lists the device's installed user applications.
func (s *Service) Apps(ctx context.Context, udid string) ([]App, error) {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return nil, err
	}
	apps, err := s.engine.ListApps(ctx, engine.DeviceID(udid))
	if err != nil {
		slog.WarnContext(ctx, "apps: engine", "udid", udid, "error", err)
		return nil, newEngineActionError("app_list_failed", err)
	}
	out := make([]App, len(apps))
	for i, app := range apps {
		out[i] = App(app)
	}
	return out, nil
}

// maxAppIconBatch matches the engine's per-call cap.
const maxAppIconBatch = 100

// AppIcons reads a batch of home-screen icons (PNG bytes by bundle id) over
// one springboard connection. Apps without a readable icon are absent.
func (s *Service) AppIcons(ctx context.Context, udid string, bundleIDs []string) (map[string][]byte, error) {
	if len(bundleIDs) == 0 || slices.Contains(bundleIDs, "") {
		return nil, &domain.ValidationError{Code: "bundle_id_required", Message: "non-empty bundle ids are required"}
	}
	if len(bundleIDs) > maxAppIconBatch {
		return nil, &domain.ValidationError{Code: "too_many_bundle_ids", Message: "too many bundle ids in one batch"}
	}
	if err := s.reachableDevice(ctx, udid); err != nil {
		return nil, err
	}
	icons, err := s.engine.AppIcons(ctx, engine.DeviceID(udid), bundleIDs)
	if err != nil {
		// The web keeps its placeholders, so the failure is only logged.
		slog.DebugContext(ctx, "app icons: engine", "udid", udid, "count", len(bundleIDs), "error", err)
		return nil, newEngineActionError("app_icons_failed", err)
	}
	return icons, nil
}

// Wallpaper fetches SpringBoard's rendered lock- or home-screen preview.
func (s *Service) Wallpaper(ctx context.Context, udid string, lockScreen bool) ([]byte, error) {
	// Loaded automatically by device cards; never wakes the phone.
	if err := s.reachableDevice(ctx, udid); err != nil {
		return nil, err
	}
	png, err := s.engine.Wallpaper(ctx, engine.DeviceID(udid), lockScreen)
	if err != nil {
		slog.DebugContext(ctx, "wallpaper: engine", "udid", udid, "error", err)
		return nil, domain.ErrNotFound
	}
	return png, nil
}

// InstallApp persists an uploaded .ipa, then serializes its device installation.
// onProgress receives each distinct phase/percent while the device works.
func (s *Service) InstallApp(ctx context.Context, udid string, ipa io.Reader, onProgress func(engine.InstallProgress)) error {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.uploads, "airvault-*.ipa")
	if err != nil {
		return fmt.Errorf("create temporary ipa: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, ipa); err != nil {
		return fmt.Errorf("store uploaded ipa: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close uploaded ipa: %w", err)
	}
	ipaPath := tmp.Name()
	err = s.runCommand(ctx, runKindInstall, udid, func(ctx context.Context) error {
		last := engine.InstallProgress{Percent: -1}
		reportChange := func(progress engine.InstallProgress) {
			if progress == last {
				return
			}
			last = progress
			onProgress(progress)
		}
		if err := s.engine.InstallApp(ctx, engine.DeviceID(udid), ipaPath, reportChange); err != nil {
			slog.DebugContext(ctx, "app install: engine", "udid", udid, "error", err)
			return newEngineActionError("app_install_failed", err)
		}
		return nil
	}, deviceWriteResource(udid))
	if err == nil {
		s.bus.Emit(events.AppCatalog, map[string]any{"udid": udid})
	}
	return err
}

// UninstallApp removes an app by bundle id. Reachable device required and the
// mutation is serialized with other device work like install.
func (s *Service) UninstallApp(ctx context.Context, udid, bundleID string) error {
	if bundleID == "" {
		return &domain.ValidationError{Code: "bundle_id_required", Message: "bundle id is required"}
	}
	if err := s.reachableDevice(ctx, udid); err != nil {
		return err
	}
	err := s.runCommand(ctx, runKindUninstall, udid, func(ctx context.Context) error {
		if err := s.engine.UninstallApp(ctx, engine.DeviceID(udid), bundleID); err != nil {
			slog.DebugContext(ctx, "app uninstall: engine", "udid", udid, "bundle", bundleID, "error", err)
			return newEngineActionError("app_uninstall_failed", err)
		}
		slog.DebugContext(ctx, "app uninstall: done", "udid", udid, "bundle", bundleID)
		return nil
	}, deviceWriteResource(udid))
	if err == nil {
		s.bus.Emit(events.AppCatalog, map[string]any{"udid": udid})
	}
	return err
}

// parseRequiredPath validates a raw device path that must name an entry (the
// root "" is not acceptable).
func parseRequiredPath(raw string) (devicefs.Path, error) {
	devicePath, err := devicefs.ParsePath(raw)
	if err != nil || devicePath.String() == "" {
		return devicefs.Path{}, &domain.ValidationError{Code: "invalid_path", Message: "path is required and must be valid"}
	}
	return devicePath, nil
}

// appDocumentsRoot maps a bundle id onto its Documents root (house_arrest).
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

// openLeasedSession opens a device filesystem session behind the reachability
// check and a device lease; release closes the session and frees the lease.
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

// FileEntry is one row of a device directory listing.
type FileEntry struct {
	Name     string             `json:"name"`
	Kind     devicefs.EntryKind `json:"kind"`
	Size     *int64             `json:"size,omitempty"`
	Modified *int64             `json:"modified,omitempty"`
}

// AppFiles lists one directory of an app's Documents container (house_arrest).
// path "" or "/" is the Documents root; only apps with file sharing enabled.
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
		out[i] = FileEntry(entry)
	}
	return out, nil
}

// AppFileDelete removes one file from an app's Documents container.
func (s *Service) AppFileDelete(ctx context.Context, udid, bundleID, devicePath string) error {
	if bundleID == "" || devicePath == "" {
		return &domain.ValidationError{Code: "app_file_selection_required", Message: "bundle id and path are required"}
	}
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

// MediaList lists one directory of the device media partition (com.apple.afc);
// path "" or "/" is the media root (DCIM, Recordings, …). Read-only.
func (s *Service) MediaList(ctx context.Context, udid, rawPath string) ([]FileEntry, error) {
	return s.deviceFileList(ctx, udid, devicefs.Media(), rawPath, "media_list_failed")
}

// ConsoleLine is one structured record from the device's os_trace stream.
type ConsoleLine struct {
	Timestamp string `json:"ts"`
	Level     string `json:"level"` // notice | info | debug | error | fault
	Pid       uint32 `json:"pid"`
	Image     string `json:"image"`
	Message   string `json:"message"`
	Subsystem string `json:"subsystem,omitempty"`
	Category  string `json:"category,omitempty"`
}

// Console streams the device's structured system log to onLine until ctx
// ends. The caller (the SSE handler) owns the transport; this only guards.
func (s *Service) Console(ctx context.Context, udid string, onLine func(ConsoleLine)) error {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return err
	}
	stream, err := s.engine.OpenConsole(ctx, engine.DeviceID(udid))
	if err != nil {
		return newEngineActionError("console_stream_failed", err)
	}
	defer stream.Close()
	for ctx.Err() == nil {
		line, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || ctx.Err() != nil {
				return nil
			}
			slog.WarnContext(ctx, "console: engine", "udid", udid, "error", err)
			return newEngineActionError("console_stream_failed", err)
		}
		onLine(ConsoleLine(line))
	}
	return nil
}
