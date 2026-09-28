package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/google/uuid"
	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
	airlog "github.com/wizier/airvault/internal/logging"
	"github.com/wizier/airvault/internal/objectstore"
)

const (
	infoPlist           = "Info.plist"
	infoPlistTimeout    = 3 * time.Minute // the app census asks for one icon per app
	maxInfoPlist        = 256 << 20
	restoreDir          = "/iTunesRestore"
	restoreApplications = restoreDir + "/RestoreApplications.plist"
)

var identityKeys = []struct{ info, lockdown string }{
	{"Build Version", "BuildVersion"},
	{"Device Name", "DeviceName"},
	{"Display Name", "DeviceName"},
	{"ICCID", "IntegratedCircuitCardIdentity"},
	{"IMEI", "InternationalMobileEquipmentIdentity"},
	{"MEID", "MobileEquipmentIdentifier"},
	{"Phone Number", "PhoneNumber"},
	{"Product Type", "ProductType"},
	{"Product Version", "ProductVersion"},
	{"Serial Number", "SerialNumber"},
}

var iTunesFiles = []string{
	"ApertureAlbumPrefs", "IC-Info.sidb", "IC-Info.sidv", "PhotosFolderAlbums", "PhotosFolderName",
	"PhotosFolderPrefs", "VoiceMemos.plist", "iPhotoAlbumPrefs", "iTunesApplicationIDs", "iTunesPrefs",
	"iTunesPrefs.plist",
}

// writeBackupInfo records the device and its App Store apps in Info.plist,
// which Finder shows and a restore reinstalls the apps from.
func (e *Engine) writeBackupInfo(ctx context.Context, udid string, session *objectstore.Session) error {
	info, err := call(ctx, infoPlistTimeout, "backup Info.plist", func(ctx context.Context) (map[string]any, error) {
		return e.backupInfo(ctx, udid)
	})
	if err != nil {
		return err
	}
	data, err := plist.MarshalIndent(info, plist.XMLFormat, "\t")
	if err != nil {
		return &Error{Kind: ErrorInternal, Detail: "encode Info.plist: " + err.Error()}
	}
	writer, err := session.Create(infoPlist)
	if err != nil {
		return storeFailure("write Info.plist", err)
	}
	if _, err := writer.Write(data); err != nil {
		writer.Abort()
		return storeFailure("write Info.plist", err)
	}
	return storeFailure("write Info.plist", writer.Commit())
}

func (e *Engine) backupInfo(ctx context.Context, udid string) (map[string]any, error) {
	session, err := e.openSession(ctx, udid)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	info := map[string]any{
		"GUID":              strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")),
		"Target Identifier": udid,
		"Target Type":       "Device",
		"Unique Identifier": strings.ToUpper(udid),
		"Last Backup Date":  time.Now().Truncate(time.Second),
	}
	values, _ := ios.Value[map[string]any](ctx, session.Lockdown, "", "")
	for _, key := range identityKeys {
		if value, _ := values[key.lockdown].(string); value != "" {
			info[key.info] = value
		}
	}
	info["iTunes Version"] = "10.0.1"
	if version, err := ios.Value[string](ctx, session.Lockdown, "com.apple.mobile.iTunes", "MinITunesVersion"); err == nil {
		info["iTunes Version"] = version
	}
	info["iTunes Settings"] = map[string]any{}
	if settings, err := ios.Value[any](ctx, session.Lockdown, "com.apple.iTunes", ""); err == nil {
		info["iTunes Settings"] = settings
	}
	e.addITunesFiles(ctx, udid, info)
	if err := e.addApplications(ctx, udid, info); err != nil {
		return nil, err
	}
	return info, nil
}

// addITunesFiles copies what the device has of the files Finder keeps.
func (e *Engine) addITunesFiles(ctx context.Context, udid string, info map[string]any) {
	files := map[string]any{}
	info["iTunes Files"] = files
	client, err := e.dialAFC(ctx, afcKey{udid: udid, source: AFCMedia})
	if err != nil {
		airlog.Component("engine").WarnContext(ctx, "no device files; Info.plist carries no iTunes files", "udid", udid, "error", err)
		return
	}
	defer client.Close()
	if data := readDeviceFile(ctx, client, "/Books/iBooksData2.plist"); len(data) > 0 {
		info["iBooks Data 2"] = data
	}
	for _, name := range iTunesFiles {
		if data := readDeviceFile(ctx, client, "/iTunes_Control/iTunes/"+name); len(data) > 0 {
			files[name] = data
		}
	}
}

// readDeviceFile reads a whole file, nil when there is none or the
// connection has failed.
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
	if closeErr := file.Close(ctx); err != nil || closeErr != nil {
		return nil
	}
	return data
}

// addApplications lists every user app and, for those from the App Store,
// what a restore needs to reinstall them.
func (e *Engine) addApplications(ctx context.Context, udid string, info map[string]any) error {
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
	installed := []any{}
	var restorable []map[string]any
	var bundleIDs []string
	for _, app := range apps {
		bundleID, _ := app["CFBundleIdentifier"].(string)
		if bundleID == "" {
			continue
		}
		installed = append(installed, bundleID)
		if app["ApplicationSINF"] != nil && app["iTunesMetadata"] != nil {
			restorable = append(restorable, app)
			bundleIDs = append(bundleIDs, bundleID)
		}
	}
	var icons map[string][]byte
	if len(bundleIDs) > 0 {
		if icons, err = e.AppIcons(ctx, DeviceID(udid), bundleIDs); err != nil {
			airlog.Component("engine").WarnContext(ctx, "no app icons; the app census goes on without them", "udid", udid, "error", err)
		}
	}
	applications := map[string]any{}
	for i, app := range restorable {
		application := map[string]any{"ApplicationSINF": app["ApplicationSINF"], "iTunesMetadata": app["iTunesMetadata"]}
		if icon := icons[bundleIDs[i]]; len(icon) > 0 {
			application["PlaceholderIcon"] = icon
		}
		applications[bundleIDs[i]] = application
	}
	info["Applications"] = applications
	info["Installed Applications"] = installed
	return nil
}

// stageRestoreApplications hands the device the snapshot's App Store apps
// to reinstall; staged reports whether the device may now hold the list.
func (e *Engine) stageRestoreApplications(ctx context.Context, udid string, session *objectstore.Session) (staged bool, err error) {
	apps, err := snapshotApplications(session)
	if err != nil || apps == nil {
		return false, err
	}
	data, err := plist.MarshalIndent(apps, plist.XMLFormat, "\t")
	if err != nil {
		return false, &Error{Kind: ErrorIntegrity, Detail: "encode RestoreApplications.plist: " + err.Error()}
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

// snapshotApplications reads Info.plist's Applications, nil when there are none.
func snapshotApplications(session *objectstore.Session) (map[string]any, error) {
	reader, err := session.Open(infoPlist)
	if errors.Is(err, fs.ErrNotExist) {
		err = fmt.Errorf("%w: the snapshot has no Info.plist", objectstore.ErrIntegrity)
	}
	if err != nil {
		return nil, storeFailure("read Info.plist", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxInfoPlist+1))
	if err == nil && len(data) > maxInfoPlist {
		err = fmt.Errorf("%w: Info.plist is larger than 256 MiB", objectstore.ErrIntegrity)
	}
	var info any
	if err == nil {
		if _, decodeErr := plist.Unmarshal(data, &info); decodeErr != nil {
			err = fmt.Errorf("%w: Info.plist: %v", objectstore.ErrIntegrity, decodeErr)
		}
	}
	if err != nil {
		return nil, storeFailure("read Info.plist", err)
	}
	root, _ := info.(map[string]any)
	apps, _ := root["Applications"].(map[string]any)
	if len(apps) == 0 {
		return nil, nil
	}
	return apps, nil
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
		airlog.Component("engine").DebugContext(ctx, "staged app list not removed", "udid", udid, "error", err)
	}
}
