package engine

import (
	"bytes"
	"context"
	"io"
	"net"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
	"github.com/wizier/airvault/internal/ios/backup2"
	"github.com/wizier/airvault/internal/ios/iostest"
	"github.com/wizier/airvault/internal/objectstore"
)

var (
	storeApp = map[string]any{"CFBundleIdentifier": "com.example.store", "ApplicationSINF": []byte("sinf"), "iTunesMetadata": []byte("meta")}
	devApp   = map[string]any{"CFBundleIdentifier": "com.example.dev"}
)

// transferFixture is a phone with the services a sync uses; each test
// scripts mobilebackup2 itself.
type transferFixture struct {
	*testPhone
	media         *iostest.FS
	done          <-chan struct{}
	cancelOnPhone chan struct{} // closed: the phone asks the observer to cancel
	finished      chan struct{} // closed when the phone hears syncDidFinish
	asserted      chan struct{} // closed at the first power assertion

	mu     sync.Mutex
	posted []string
}

func newTransferFixture(t *testing.T) *transferFixture {
	t.Helper()
	f := &transferFixture{testPhone: newTestPhone(t), media: iostest.NewFS(), done: t.Context().Done(),
		cancelOnPhone: make(chan struct{}), finished: make(chan struct{}), asserted: make(chan struct{})}
	var assertOnce sync.Once
	f.phone.Handle(afc.Service, f.media.Serve)
	f.phone.Handle(ios.NotificationProxyService, f.notifications)
	f.phone.Handle(ios.AssertionAgentService, servePlist(plist.BinaryFormat, func(request map[string]any) map[string]any {
		if request["AssertionTypeKey"] == "AMDPowerAssertionTypeWirelessSync" {
			assertOnce.Do(func() { close(f.asserted) })
		}
		return map[string]any{"CommandKey": request["CommandKey"]}
	}))
	f.phone.Handle(ios.InstallationProxyService, browseApps(storeApp, devApp))
	f.phone.Handle(ios.SpringBoardService, xmlService(func(request map[string]any) map[string]any {
		return map[string]any{"pngData": []byte("PNG-" + request["bundleId"].(string))}
	}))
	return f
}

func (f *transferFixture) notifications(raw net.Conn) {
	conn := ios.NewPlistConn(raw, plist.XMLFormat)
	for {
		var request map[string]any
		if conn.Recv(&request) != nil {
			return
		}
		name, _ := request["Name"].(string)
		switch {
		case request["Command"] == "PostNotification":
			f.mu.Lock()
			f.posted = append(f.posted, name)
			f.mu.Unlock()
			if name == syncDidFinish || name == syncFailed {
				close(f.finished)
			}
		case name == syncCancelRequest:
			select {
			case <-f.cancelOnPhone:
				_ = conn.Send(map[string]any{"Command": "RelayNotification", "Name": name})
			case <-f.done:
				return
			}
		}
	}
}

// syncEnded waits for the sync session's end and returns what was posted.
func (f *transferFixture) syncEnded(t *testing.T) []string {
	t.Helper()
	select {
	case <-f.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the sync session never finished")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posted)
}

func (f *transferFixture) mobileBackup(t *testing.T, script func(dl *iostest.DeviceLink)) {
	f.phone.Handle(backup2.Service, iostest.Backup2(t, script))
}

// browseApps answers Browse in two batches, as installd pages its answer.
func browseApps(apps ...map[string]any) iostest.Handler {
	return func(raw net.Conn) {
		conn := ios.NewPlistConn(raw, plist.XMLFormat)
		var request map[string]any
		if conn.Recv(&request) != nil || request["Command"] != "Browse" {
			return
		}
		half := len(apps) / 2
		_ = conn.Send(map[string]any{"Status": "BrowsingProgress", "CurrentList": apps[:half]})
		_ = conn.Send(map[string]any{"Status": "Complete", "CurrentList": apps[half:]})
		_ = conn.Recv(&request)
	}
}

func (f *transferFixture) backup(ctx context.Context, onProgress func(Progress)) (string, int64, error) {
	id := uuid.NewString()
	added, err := f.engine.BuildSnapshot(ctx, BuildSnapshotRequest{DeviceID: f.udid, SnapshotID: SnapshotID(id)}, onProgress)
	return id, added, err
}

func (f *transferFixture) publish(t *testing.T, source string, files map[string][]byte) SnapshotRef {
	t.Helper()
	id := uuid.NewString()
	session, err := f.objects.BeginSnapshot(source, id, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		writer, err := session.Create(name)
		if err == nil {
			_, err = writer.Write(data)
		}
		if err == nil {
			err = writer.Commit()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := session.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	view, err := f.objects.OpenStaging(source, id)
	if err == nil {
		_, err = f.objects.Publish(view)
	}
	if err != nil {
		t.Fatal(err)
	}
	return SnapshotRef{SourceID: DeviceID(source), SnapshotID: SnapshotID(id)}
}

func readSnapshotFile(t *testing.T, view *objectstore.StagingView, name string) []byte {
	t.Helper()
	file, err := view.Open(name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBuildSnapshot(t *testing.T) {
	f := newTransferFixture(t)
	manifest := bytes.Repeat([]byte("m"), 300<<10)
	f.mobileBackup(t, func(dl *iostest.DeviceLink) {
		request := dl.Request()
		if request["MessageName"] != "Backup" || request["TargetIdentifier"] != "PHONE-UDID" || request["SourceIdentifier"] != "PHONE-UDID" {
			t.Errorf("backup request = %v", request)
		}
		if _, code, _ := dl.Download(5, "PHONE-UDID/Status.plist"); code != -13 {
			t.Errorf("a first backup found a Status.plist: status %d", code)
		}
		dl.Upload(60,
			iostest.UploadFile{Path: "PHONE-UDID/Manifest.db", Data: manifest},
			iostest.UploadFile{Path: "/.b/1/Status.plist", Data: []byte("status")},
			iostest.UploadFile{Path: ".b/2/scratch", Data: []byte("device scratch")},
		)
		if code, _ := dl.Ask("DLMessageMoveFiles", map[string]any{"/.b/1/Status.plist": "PHONE-UDID/Status.plist"}, map[string]any{}, 90.0); code != 0 {
			t.Errorf("move status = %d", code)
		}
		select {
		case <-f.asserted:
		case <-time.After(5 * time.Second):
			t.Error("the phone was not kept awake during the transfer")
		}
		dl.Finish(0, "")
	})
	f.phone.SetValue("", "SerialNumber", "F2LTEST")
	f.phone.SetValue("com.apple.mobile.iTunes", "MinITunesVersion", "12.13")
	f.phone.SetValue("com.apple.iTunes", "SyncDataWithCloud", true)
	f.media.WriteFile("iTunes_Control/iTunes/iTunesPrefs", []byte("prefs"))

	var mu sync.Mutex
	var last Progress
	id, added, err := f.backup(context.Background(), func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		last = p
	})
	if err != nil {
		t.Fatal(err)
	}
	if added < int64(len(manifest)) {
		t.Errorf("added = %d, want at least the manifest's %d bytes", added, len(manifest))
	}
	if posted, want := f.syncEnded(t), []string{syncWillStart, syncLockRequest, syncDidStart, syncDidFinish}; !reflect.DeepEqual(posted, want) {
		t.Errorf("posted %v, want %v", posted, want)
	}
	if f.media.OpenHandles() != 0 {
		t.Error("the sync lock file was left open")
	}
	mu.Lock()
	if want := (Progress{Phase: ProgressPhaseSealing, Percent: 90, BytesDone: int64(len(manifest) + len("status") + len("device scratch"))}); last != want {
		t.Errorf("last progress = %+v, want %+v", last, want)
	}
	mu.Unlock()

	view, err := f.objects.OpenStaging("PHONE-UDID", id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readSnapshotFile(t, view, "Manifest.db"), manifest) || string(readSnapshotFile(t, view, "Status.plist")) != "status" {
		t.Error("the snapshot does not hold what the phone sent")
	}
	if _, found := view.FileSize(objectstore.ProtocolDir + "/.b/2/scratch"); found {
		t.Error("the device's scratch space reached the snapshot")
	}
	var info map[string]any
	if _, err := plist.Unmarshal(readSnapshotFile(t, view, "Info.plist"), &info); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"Device Name": "Test iPhone", "Serial Number": "F2LTEST", "Target Identifier": "PHONE-UDID",
		"Target Type": "Device", "iTunes Version": "12.13",
		"iTunes Settings":        map[string]any{"SyncDataWithCloud": true},
		"iTunes Files":           map[string]any{"iTunesPrefs": []byte("prefs")},
		"Installed Applications": []any{"com.example.store", "com.example.dev"},
		"Applications": map[string]any{"com.example.store": map[string]any{
			"ApplicationSINF": []byte("sinf"), "iTunesMetadata": []byte("meta"), "PlaceholderIcon": []byte("PNG-com.example.store"),
		}},
	} {
		if !reflect.DeepEqual(info[key], want) {
			t.Errorf("Info.plist %s = %#v, want %#v", key, info[key], want)
		}
	}
}

// A path outside the source is refused to the device and fails the backup.
func TestBuildSnapshotConfinesTheDevice(t *testing.T) {
	f := newTransferFixture(t)
	f.mobileBackup(t, func(dl *iostest.DeviceLink) {
		dl.Request()
		if received, code, _ := dl.Download(5, "../OTHER-UDID/Manifest.db"); len(received) != 0 || code != -13 {
			t.Errorf("an escaping read got %v, status %d", received, code)
		}
		dl.Finish(0, "")
	})
	if _, _, err := f.backup(context.Background(), nil); kindOf(err) != ErrorIntegrity {
		t.Fatalf("err = %v, want an integrity failure", err)
	}
}

func TestBuildSnapshotOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		device func(dl *iostest.DeviceLink)
		want   ErrorKind
	}{
		{"passcode not entered", func(dl *iostest.DeviceLink) { dl.Finish(backup2.CodeDeviceLocked, "not confirmed") }, ErrorBackupNotConfirmed},
		{"wrong password", func(dl *iostest.DeviceLink) { dl.Finish(backup2.CodeWrongPassword, "wrong") }, ErrorInvalidBackupPassword},
		{"other refusal", func(dl *iostest.DeviceLink) { dl.Finish(1, "failed") }, ErrorProtocol},
		{"no verdict", func(dl *iostest.DeviceLink) { dl.Send("DLMessageDisconnect", "bye") }, ErrorProtocol},
		{"dropped", func(dl *iostest.DeviceLink) {}, ErrorConnectionLost},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTransferFixture(t)
			f.mobileBackup(t, func(dl *iostest.DeviceLink) {
				dl.Request()
				c.device(dl)
			})
			if _, _, err := f.backup(context.Background(), nil); kindOf(err) != c.want {
				t.Fatalf("err = %v, want kind %d", err, c.want)
			}
			f.syncEnded(t)
		})
	}
}

func TestBuildSnapshotCancel(t *testing.T) {
	for name, onPhone := range map[string]bool{"by the caller": false, "on the phone": true} {
		t.Run(name, func(t *testing.T) {
			f := newTransferFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.mobileBackup(t, func(dl *iostest.DeviceLink) {
				dl.Request()
				if onPhone {
					close(f.cancelOnPhone)
				} else {
					cancel()
				}
				if !dl.Wait() {
					t.Error("the host left without DLMessageDisconnect")
				}
			})
			if _, _, err := f.backup(ctx, nil); kindOf(err) != ErrorCancelled {
				t.Fatalf("err = %v", err)
			}
			if posted := f.syncEnded(t); posted[len(posted)-1] != syncDidFinish || f.media.OpenHandles() != 0 {
				t.Fatal("the sync session was left behind")
			}
		})
	}
}

// Another host's sync holds the lock for a moment; the backup waits for it.
// One cancelled while waiting never started, and tells the phone so.
func TestBuildSnapshotWaitsForTheSyncLock(t *testing.T) {
	f := newTransferFixture(t)
	f.mobileBackup(t, func(dl *iostest.DeviceLink) {
		dl.Request()
		dl.Finish(0, "")
	})
	f.media.Lock(syncLockFile, true)
	time.AfterFunc(300*time.Millisecond, func() { f.media.Lock(syncLockFile, false) })
	if _, _, err := f.backup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	waiting := newTransferFixture(t)
	waiting.media.Lock(syncLockFile, true)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	if _, _, err := waiting.backup(ctx, nil); kindOf(err) != ErrorCancelled {
		t.Fatalf("err = %v", err)
	}
	if posted := waiting.syncEnded(t); posted[len(posted)-1] != syncFailed || slices.Contains(posted, syncDidStart) {
		t.Fatalf("posted %v, want the sync to fail to start", posted)
	}
}

func restoreInfo(t *testing.T) []byte {
	info, err := plist.Marshal(map[string]any{"Applications": map[string]any{
		"com.example.store": map[string]any{"ApplicationSINF": []byte("sinf"), "iTunesMetadata": []byte("meta")},
	}}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestRestoreSnapshot(t *testing.T) {
	f := newTransferFixture(t)
	ref := f.publish(t, "OLD-PHONE", map[string][]byte{"Manifest.db": []byte("db"), "Info.plist": restoreInfo(t)})
	f.phone.SetValue("com.apple.fmip", "IsAssociated", false)
	f.mobileBackup(t, func(dl *iostest.DeviceLink) {
		request := dl.Request()
		if request["MessageName"] != "Restore" || request["TargetIdentifier"] != "PHONE-UDID" || request["SourceIdentifier"] != "OLD-PHONE" {
			t.Errorf("restore request = %v", request)
		}
		wantOptions := map[string]any{"RestoreShouldReboot": true, "RestoreDontCopyBackup": true, "RestorePreserveSettings": false,
			"RestoreSystemFiles": false, "RemoveItemsNotRestored": true, "Password": "secret"}
		if options := request["Options"]; !reflect.DeepEqual(options, wantOptions) {
			t.Errorf("options = %v, want %v", options, wantOptions)
		}
		if received, code, _ := dl.Download(50, "OLD-PHONE/Manifest.db"); string(received["OLD-PHONE/Manifest.db"]) != "db" || code != 0 {
			t.Errorf("download = %q, status %d", received, code)
		}
		dl.Finish(0, "")
	})
	err := f.engine.RestoreSnapshot(context.Background(), RestoreSnapshotRequest{TargetID: f.udid, Snapshot: ref, Password: "secret",
		Reboot: true, SettingsFromBackup: true, RemoveItemsNotRestored: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	staged, ok := f.media.ReadFile(restoreApplications)
	var apps map[string]any
	if _, err := plist.Unmarshal(staged, &apps); !ok || err != nil || apps["com.example.store"] == nil {
		t.Fatalf("staged app list = %q", staged)
	}
	f.syncEnded(t)
}

// A failed restore takes its app list back off the phone.
func TestRestoreSnapshotFailures(t *testing.T) {
	cases := []struct {
		name string
		code int
		want ErrorKind
	}{
		{"passcode not entered", backup2.CodeDeviceLocked, ErrorDeviceLocked},
		{"refused", 1, ErrorProtocol},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTransferFixture(t)
			ref := f.publish(t, "PHONE-UDID", map[string][]byte{"Info.plist": restoreInfo(t)})
			f.mobileBackup(t, func(dl *iostest.DeviceLink) {
				dl.Request()
				dl.Finish(c.code, c.name)
			})
			err := f.engine.RestoreSnapshot(context.Background(), RestoreSnapshotRequest{TargetID: f.udid, Snapshot: ref}, nil)
			if kindOf(err) != c.want {
				t.Fatalf("err = %v, want kind %d", err, c.want)
			}
			if f.media.Exists(restoreDir) {
				t.Fatal("the staged app list was left on the phone")
			}
		})
	}
}

func TestRestoreSnapshotRefusedWhileFindMyIsOn(t *testing.T) {
	f := newTransferFixture(t)
	ref := f.publish(t, "PHONE-UDID", map[string][]byte{"Info.plist": restoreInfo(t)})
	f.phone.SetValue("com.apple.fmip", "IsAssociated", true)
	f.mobileBackup(t, func(dl *iostest.DeviceLink) { t.Error("restore started with Find My on") })
	err := f.engine.RestoreSnapshot(context.Background(), RestoreSnapshotRequest{TargetID: f.udid, Snapshot: ref}, nil)
	if kindOf(err) != ErrorFindMyEnabled {
		t.Fatalf("err = %v", err)
	}
	if f.phone.Dialed(backup2.Service) != 0 || f.phone.Dialed(ios.NotificationProxyService) != 0 {
		t.Fatal("the phone was touched")
	}
}

func TestChangeBackupPassword(t *testing.T) {
	encrypted := func(on bool) BackupPasswordResult { return BackupPasswordResult{EncryptionKnown: true, Encrypted: on} }
	cases := []struct {
		name     string
		old, new string
		before   bool
		device   func(phone *iostest.Device, dl *iostest.DeviceLink)
		want     BackupPasswordResult
		kind     ErrorKind
	}{
		{"enable", "", "pw", false, func(phone *iostest.Device, dl *iostest.DeviceLink) {
			phone.SetValue("com.apple.mobile.backup", "WillEncrypt", true)
			dl.Finish(0, "")
		}, encrypted(true), 0},
		{"wrong password", "bad", "pw", true, func(phone *iostest.Device, dl *iostest.DeviceLink) {
			dl.Finish(backup2.CodeWrongPassword, "wrong password")
		}, encrypted(true), ErrorInvalidBackupPassword},
		{"disabled without a verdict", "pw", "", true, func(phone *iostest.Device, dl *iostest.DeviceLink) {
			phone.SetValue("com.apple.mobile.backup", "WillEncrypt", false)
			dl.Send("DLMessageDisconnect", "bye")
		}, encrypted(false), 0},
		{"no verdict, no change", "pw", "", true, func(phone *iostest.Device, dl *iostest.DeviceLink) {
			dl.Send("DLMessageDisconnect", "bye")
		}, encrypted(true), ErrorOutcomeUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newTestPhone(t)
			p.phone.SetValue("com.apple.mobile.backup", "WillEncrypt", c.before)
			p.phone.Handle(backup2.Service, iostest.Backup2(t, func(dl *iostest.DeviceLink) {
				request := dl.Request()
				if request["MessageName"] != "ChangePassword" || request["TargetIdentifier"] != "PHONE-UDID" {
					t.Errorf("request = %v", request)
				}
				c.device(p.phone, dl)
				dl.Wait()
			}))
			result, err := p.engine.ChangeBackupPassword(context.Background(), p.udid, c.old, c.new)
			if kindOf(err) != c.kind || result != c.want {
				t.Fatalf("result = %+v, %v; want %+v, kind %d", result, err, c.want, c.kind)
			}
		})
	}
}

func TestBackupKey(t *testing.T) {
	cases := map[string]string{
		"PHONE/Manifest.db":           "Manifest.db",
		"/PHONE//ab/./abc":            "ab/abc",
		"PHONE/../PHONE/Status.plist": "PHONE/Status.plist",
		"/.b/6/x":                     objectstore.ProtocolDir + "/.b/6/x",
		"PHONE":                       "",
	}
	for devicePath, want := range cases {
		if key, err := backupKey("PHONE", devicePath); err != nil || key != want {
			t.Errorf("backupKey(%q) = %q, %v; want %q", devicePath, key, err, want)
		}
	}
	for _, devicePath := range []string{
		"", "/", "OTHER/Manifest.db", "../OTHER/x", "PHONE/" + objectstore.ProtocolDir + "/x",
		"PHONE/a\\b", "PHONE/a\x00b", "PHONE/\u2028", "PHONE/\xff",
		"PHONE/" + string(bytes.Repeat([]byte("a"), 256)),
		"PHONE" + string(bytes.Repeat([]byte("/a"), 128)),
		"PHONE/" + string(bytes.Repeat([]byte("a/"), 2100)),
	} {
		if key, err := backupKey("PHONE", devicePath); err == nil {
			t.Errorf("backupKey(%.40q) = %q, want a refusal", devicePath, key)
		}
	}
}
