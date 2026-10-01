package engine

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/objectstore"
)

const (
	infoPlistTimeout    = 3 * time.Minute // the app census asks for one icon per app
	restoreDir          = "/iTunesRestore"
	restoreApplications = restoreDir + "/RestoreApplications.plist"
)

var iTunesFiles = []string{
	"ApertureAlbumPrefs", "IC-Info.sidb", "IC-Info.sidv", "PhotosFolderAlbums", "PhotosFolderName",
	"PhotosFolderPrefs", "VoiceMemos.plist", "iPhotoAlbumPrefs", "iTunesApplicationIDs", "iTunesPrefs",
	"iTunesPrefs.plist",
}

// writeBackupInfo records the device and its App Store apps in the
// snapshot's Info.plist.
func (e *Engine) writeBackupInfo(ctx context.Context, udid string, draft *objectstore.Draft) error {
	info, err := call(ctx, infoPlistTimeout, "backup Info.plist", func(ctx context.Context) (*iosbackup.Info, error) {
		return e.backupInfo(ctx, udid)
	})
	if err != nil {
		return err
	}
	return storeFailure("write Info.plist", iosbackup.WriteInfo(draft, info))
}

func (e *Engine) backupInfo(ctx context.Context, udid string) (*iosbackup.Info, error) {
	session, err := e.openSession(ctx, udid)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	values, _ := session.Value[map[string]any](ctx, "", "")
	value := func(key string) string {
		text, _ := values[key].(string)
		return text
	}
	info := &iosbackup.Info{
		BuildVersion:     value("BuildVersion"),
		DeviceName:       value("DeviceName"),
		DisplayName:      value("DeviceName"),
		GUID:             strings.ToUpper(strings.ReplaceAll(uuid.NewV4().String(), "-", "")),
		ICCID:            value("IntegratedCircuitCardIdentity"),
		IMEI:             value("InternationalMobileEquipmentIdentity"),
		MEID:             value("MobileEquipmentIdentifier"),
		PhoneNumber:      value("PhoneNumber"),
		ProductType:      value("ProductType"),
		ProductVersion:   value("ProductVersion"),
		SerialNumber:     value("SerialNumber"),
		TargetIdentifier: udid,
		TargetType:       "Device",
		UniqueIdentifier: strings.ToUpper(udid),
		LastBackupDate:   time.Now().Truncate(time.Second),
		ITunesVersion:    "10.0.1",
		ITunesSettings:   map[string]any{},
	}
	if version, err := session.Value[string](ctx, "com.apple.mobile.iTunes", "MinITunesVersion"); err == nil {
		info.ITunesVersion = version
	}
	if settings, err := session.Value[any](ctx, "com.apple.iTunes", ""); err == nil {
		info.ITunesSettings = settings
	}
	e.addITunesFiles(ctx, udid, info)
	if err := e.addApplications(ctx, udid, info); err != nil {
		return nil, err
	}
	return info, nil
}

// addITunesFiles copies what the device has of the files Finder keeps.
func (e *Engine) addITunesFiles(ctx context.Context, udid string, info *iosbackup.Info) {
	info.ITunesFiles = map[string][]byte{}
	client, err := e.dialAFC(ctx, afcKey{udid: udid, source: AFCMedia})
	if err != nil {
		slog.WarnContext(ctx, "no device files; Info.plist carries no iTunes files", "udid", udid, "error", err)
		return
	}
	defer client.Close()
	info.IBooksData = readDeviceFile(ctx, client, "/Books/iBooksData2.plist")
	for _, name := range iTunesFiles {
		if data := readDeviceFile(ctx, client, "/iTunes_Control/iTunes/"+name); data != nil {
			info.ITunesFiles[name] = data
		}
	}
}

// readDeviceFile reads a whole file, nil when there is none, it is empty or
// the connection has failed.
func readDeviceFile(ctx context.Context, client *afc.Client, path string) []byte {
	if client.Torn() {
		return nil
	}
	info, err := statFile(ctx, client, path)
	if err != nil {
		return nil
	}
	file, err := client.Open(ctx, path, afc.ReadOnly)
	if err != nil {
		return nil
	}
	data, err := readAll(ctx, file, info.Size)
	if closeErr := file.Close(ctx); err != nil || closeErr != nil || len(data) == 0 {
		return nil
	}
	return data
}

// addApplications lists every user app and, for those from the App Store,
// what a restore needs to reinstall them.
func (e *Engine) addApplications(ctx context.Context, udid string, info *iosbackup.Info) error {
	var apps []map[string]any
	err := e.installationProxy(ctx, DeviceID(udid), func(proxy *ios.InstallationProxy) (err error) {
		apps, err = proxy.Browse(ctx, map[string]any{
			"ApplicationType":  "User",
			"ReturnAttributes": []any{"CFBundleIdentifier", "ApplicationSINF", "iTunesMetadata"},
		})
		return err
	})
	if err != nil {
		return err
	}
	info.InstalledApplications = []string{}
	info.Applications = map[string]iosbackup.Application{}
	var storeApps []string
	for _, app := range apps {
		bundleID, _ := app["CFBundleIdentifier"].(string)
		if bundleID == "" {
			continue
		}
		info.InstalledApplications = append(info.InstalledApplications, bundleID)
		if app["ApplicationSINF"] != nil && app["iTunesMetadata"] != nil {
			info.Applications[bundleID] = iosbackup.Application{SINF: app["ApplicationSINF"], Metadata: app["iTunesMetadata"]}
			storeApps = append(storeApps, bundleID)
		}
	}
	if len(storeApps) == 0 {
		return nil
	}
	icons, err := e.AppIcons(ctx, DeviceID(udid), storeApps)
	if err != nil {
		slog.WarnContext(ctx, "no app icons; the app census goes on without them", "udid", udid, "error", err)
	}
	for bundleID, icon := range icons {
		application := info.Applications[bundleID]
		application.PlaceholderIcon = icon
		info.Applications[bundleID] = application
	}
	return nil
}

// stageRestoreApplications hands the device the backup's App Store apps to
// reinstall; staged reports whether the device may now hold the list.
func (e *Engine) stageRestoreApplications(ctx context.Context, udid string, from *iosbackup.Backup) (staged bool, err error) {
	data, err := from.RestoreApplications()
	if err != nil {
		return false, &Error{Kind: ErrorIntegrity, Detail: "encode RestoreApplications.plist: " + err.Error()}
	}
	if data == nil {
		return false, nil
	}
	return true, do(ctx, deviceWorkTimeout, "stage RestoreApplications.plist", func(ctx context.Context) error {
		client, err := e.dialAFC(ctx, afcKey{udid: udid, source: AFCMedia})
		if err != nil {
			return err
		}
		defer client.Close()
		_ = client.MakeDir(ctx, restoreDir)
		file, err := client.Open(ctx, restoreApplications, afc.WriteOnly)
		if err != nil {
			return err
		}
		if err := file.Write(ctx, data); err != nil {
			return err
		}
		return file.Close(ctx)
	})
}

// removeRestoreApplications undoes stageRestoreApplications, best effort.
func (e *Engine) removeRestoreApplications(ctx context.Context, udid string) {
	err := do(ctx, connectTimeout, "remove RestoreApplications.plist", func(ctx context.Context) error {
		client, err := e.dialAFC(ctx, afcKey{udid: udid, source: AFCMedia})
		if err != nil {
			return err
		}
		defer client.Close()
		return client.RemoveAll(ctx, restoreDir)
	})
	if err != nil {
		slog.DebugContext(ctx, "staged app list not removed", "udid", udid, "error", err)
	}
}
