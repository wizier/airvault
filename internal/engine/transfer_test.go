package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
	"github.com/wizier/airvault/internal/ios/backup2"
	"github.com/wizier/airvault/internal/ios/iostest"
	"github.com/wizier/airvault/internal/iosbackup"
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
	root          string
	objects       *objectstore.Store
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
	f.root = t.TempDir()
	objects, err := objectstore.New(f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Close() })
	f.objects = objects
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

func (f *transferFixture) backup(ctx context.Context, onProgress func(Progress)) (*objectstore.StagingView, error) {
	session, err := f.objects.BeginSnapshot(string(f.udid), uuid.New().String(), nil)
	if err != nil {
		return nil, err
	}
	return f.engine.BuildSnapshot(ctx, f.udid, session, onProgress)
}

// publish stores a complete backup of source holding files besides the
// plists a restore needs.
func (f *transferFixture) publish(t *testing.T, source string, files map[string][]byte) *iosbackup.Backup {
	t.Helper()
	session, err := f.objects.BeginSnapshot(source, uuid.New().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	plists := map[string]any{
		"Manifest.plist": map[string]any{"IsEncrypted": false},
		"Status.plist":   map[string]any{"SnapshotState": "finished"},
	}
	for name, value := range plists {
		files[name], _ = plist.Marshal(value, plist.XMLFormat)
	}
	info := &iosbackup.Info{TargetIdentifier: source, TargetType: "Device", ITunesSettings: map[string]any{},
		Applications: map[string]iosbackup.Application{"com.example.store": {SINF: []byte("sinf"), Metadata: []byte("meta")}}}
	if err := iosbackup.WriteInfo(session, info); err != nil {
		t.Fatal(err)
	}
	files["Manifest.db"] = append(files["Manifest.db"], "db"...)
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
	staged, err := session.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.objects.Publish(staged)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := iosbackup.Open(view)
	if err != nil {
		t.Fatal(err)
	}
	return backup
}

func readSnapshotFile(t *testing.T, view *objectstore.View, name string) []byte {
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
	staged, err := f.backup(context.Background(), func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		last = p
	})
	if err != nil {
		t.Fatal(err)
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

	view := &staged.View
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
	if _, err := f.backup(context.Background(), nil); kindOf(err) != ErrorIntegrity {
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
			if _, err := f.backup(context.Background(), nil); kindOf(err) != c.want {
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
			if _, err := f.backup(ctx, nil); kindOf(err) != ErrorCancelled {
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
	if _, err := f.backup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	waiting := newTransferFixture(t)
	waiting.media.Lock(syncLockFile, true)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	if _, err := waiting.backup(ctx, nil); kindOf(err) != ErrorCancelled {
		t.Fatalf("err = %v", err)
	}
	if posted := waiting.syncEnded(t); posted[len(posted)-1] != syncFailed || slices.Contains(posted, syncDidStart) {
		t.Fatalf("posted %v, want the sync to fail to start", posted)
	}
}

func TestRestoreSnapshot(t *testing.T) {
	f := newTransferFixture(t)
	backup := f.publish(t, "OLD-PHONE", map[string][]byte{})
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
	err := f.engine.RestoreSnapshot(context.Background(), f.udid, backup, RestoreOptions{Password: "secret",
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
			backup := f.publish(t, "PHONE-UDID", map[string][]byte{})
			f.mobileBackup(t, func(dl *iostest.DeviceLink) {
				dl.Request()
				dl.Finish(c.code, c.name)
			})
			err := f.engine.RestoreSnapshot(context.Background(), f.udid, backup, RestoreOptions{}, nil)
			if kindOf(err) != c.want {
				t.Fatalf("err = %v, want kind %d", err, c.want)
			}
			if f.media.Exists(restoreDir) {
				t.Fatal("the staged app list was left on the phone")
			}
		})
	}
}

// A restore that meets a missing object fails as integrity and keeps the
// store's error, so the restore point can be marked damaged.
func TestRestoreSnapshotKeepsTheDamage(t *testing.T) {
	f := newTransferFixture(t)
	backup := f.publish(t, "PHONE-UDID", map[string][]byte{"photo": []byte("photo")})
	sum := sha256.Sum256([]byte("photo"))
	ref := hex.EncodeToString(sum[:])
	if err := os.Remove(filepath.Join(f.root, "PHONE-UDID", "objects", ref[:2], ref)); err != nil {
		t.Fatal(err)
	}
	f.mobileBackup(t, func(dl *iostest.DeviceLink) {
		dl.Request()
		dl.Download(50, "PHONE-UDID/photo")
		dl.Finish(0, "")
	})
	err := f.engine.RestoreSnapshot(context.Background(), f.udid, backup, RestoreOptions{}, nil)
	if kindOf(err) != ErrorIntegrity || !errors.Is(err, objectstore.ErrIntegrity) {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreSnapshotRefusedWhileFindMyIsOn(t *testing.T) {
	f := newTransferFixture(t)
	backup := f.publish(t, "PHONE-UDID", map[string][]byte{})
	f.phone.SetValue("com.apple.fmip", "IsAssociated", true)
	f.mobileBackup(t, func(dl *iostest.DeviceLink) { t.Error("restore started with Find My on") })
	err := f.engine.RestoreSnapshot(context.Background(), f.udid, backup, RestoreOptions{}, nil)
	if kindOf(err) != ErrorFindMyEnabled {
		t.Fatalf("err = %v", err)
	}
	if f.phone.Dialed(backup2.Service) != 0 || f.phone.Dialed(ios.NotificationProxyService) != 0 {
		t.Fatal("the phone was touched")
	}
}
