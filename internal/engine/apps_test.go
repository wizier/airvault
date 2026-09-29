package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
	"github.com/wizier/airvault/internal/ios/iostest"
)

func TestListApps(t *testing.T) {
	p := newTestPhone(t)
	p.phone.Handle(ios.InstallationProxyService, xmlService(func(request map[string]any) map[string]any {
		options, _ := request["ClientOptions"].(map[string]any)
		attributes, _ := options["ReturnAttributes"].([]any)
		if request["Command"] != "Lookup" || options["ApplicationType"] != "User" || len(attributes) != len(appAttributes) {
			return map[string]any{"Error": "BadRequest"}
		}
		return map[string]any{"LookupResult": map[string]any{
			"com.b.notes":  map[string]any{"CFBundleName": "notes", "CFBundleVersion": "7"},
			"com.a.files":  map[string]any{"CFBundleDisplayName": "Files", "CFBundleShortVersionString": "2.1", "UIFileSharingEnabled": true},
			"com.c.bare":   map[string]any{},
			"com.d.broken": "not a dictionary",
		}}
	}))
	apps, err := p.engine.ListApps(context.Background(), p.udid)
	if err != nil {
		t.Fatal(err)
	}
	want := []App{
		{BundleID: "com.c.bare", Name: "com.c.bare"},
		{BundleID: "com.a.files", Name: "Files", Version: "2.1", FileSharing: true},
		{BundleID: "com.b.notes", Name: "notes", Version: "7"},
	}
	if !reflect.DeepEqual(apps, want) {
		t.Fatalf("apps = %+v\nwant %+v", apps, want)
	}
}

// An icon the phone cannot give is skipped; a dropped connection ends the
// batch with the icons already read.
func TestAppIcons(t *testing.T) {
	p := newTestPhone(t)
	p.phone.Handle(ios.SpringBoardService, xmlService(func(request map[string]any) map[string]any {
		switch request["bundleId"] {
		case "com.a":
			return map[string]any{"pngData": []byte("PNG-A")}
		case "com.missing":
			return map[string]any{"Error": "NotFound"}
		case "com.empty":
			return map[string]any{"pngData": []byte{}}
		}
		return nil // drop the connection
	}))
	icons, err := p.engine.AppIcons(context.Background(), p.udid, []string{"com.a", "com.missing", "com.empty", "com.drop", "com.after"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]byte{"com.a": []byte("PNG-A")}; !reflect.DeepEqual(icons, want) {
		t.Fatalf("icons = %v, want %v", icons, want)
	}
}

type installFixture struct {
	*testPhone
	media     *iostest.FS
	ipa       string
	mu        sync.Mutex
	installed []byte // what installd found at PackagePath
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	f := &installFixture{testPhone: newTestPhone(t), media: iostest.NewFS()}
	f.phone.Handle(afc.Service, f.media.Serve)
	f.phone.Handle(ios.InstallationProxyService, xmlService(func(request map[string]any) map[string]any {
		switch request["Command"] {
		case "Install":
			f.mu.Lock()
			f.installed, _ = f.media.ReadFile(request["PackagePath"].(string))
			f.mu.Unlock()
			return map[string]any{"Status": "Complete", "PercentComplete": 100}
		case "Uninstall":
			if request["ApplicationIdentifier"] == "com.example.gone" {
				return map[string]any{"Error": "APIInternalError", "ErrorDescription": "not installed"}
			}
			return map[string]any{"Status": "Complete"}
		}
		return nil
	}))
	f.ipa = filepath.Join(t.TempDir(), "app.ipa")
	if err := os.WriteFile(f.ipa, bytes.Repeat([]byte("ipa"), afc.MaxTransfer), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestInstallApp(t *testing.T) {
	f := newInstallFixture(t)
	var mu sync.Mutex
	var phases []InstallProgress
	err := f.engine.InstallApp(context.Background(), f.udid, f.ipa, func(progress InstallProgress) {
		mu.Lock()
		defer mu.Unlock()
		phases = append(phases, progress)
	})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(f.ipa)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !bytes.Equal(f.installed, want) {
		t.Fatalf("installd saw %d bytes, want the %d-byte package", len(f.installed), len(want))
	}
	if f.media.Exists(stagedIPA) || f.media.OpenHandles() != 0 {
		t.Fatal("the staged package was left on the phone")
	}
	mu.Lock()
	defer mu.Unlock()
	if last := phases[len(phases)-1]; last != (InstallProgress{Phase: InstallPhaseInstalling, Percent: 100}) {
		t.Fatalf("last progress = %+v", last)
	}
}

// A full phone is reported as such, before installd is ever asked.
func TestInstallAppOnAFullPhone(t *testing.T) {
	f := newInstallFixture(t)
	f.media.SetFull(true)
	err := f.engine.InstallApp(context.Background(), f.udid, f.ipa, nil)
	if kindOf(err) != ErrorDeviceStorageFull {
		t.Fatalf("full phone: %v, want ErrorDeviceStorageFull", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installed != nil || f.media.Exists(stagedIPA) {
		t.Fatal("a failed upload must not install and must be cleaned up")
	}
}

// Cancelling during the upload stops it and still removes the partial copy.
func TestInstallAppUploadIsCancellable(t *testing.T) {
	f := newInstallFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	err := f.engine.InstallApp(ctx, f.udid, f.ipa, func(progress InstallProgress) {
		if progress.Phase == InstallPhaseStaging && progress.Percent > 0 {
			cancel()
		}
	})
	if kindOf(err) != ErrorCancelled {
		t.Fatalf("cancelled upload: %v, want ErrorCancelled", err)
	}
	if f.media.Exists(stagedIPA) {
		t.Fatal("the partial upload was left on the phone")
	}
}

func TestUninstallApp(t *testing.T) {
	f := newInstallFixture(t)
	ctx := context.Background()
	if err := f.engine.UninstallApp(ctx, f.udid, "com.example.app"); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.UninstallApp(ctx, f.udid, "com.example.gone"); kindOf(err) != ErrorProtocol {
		t.Fatalf("failed uninstall: %v, want ErrorProtocol", err)
	}
	if err := f.engine.UninstallApp(ctx, f.udid, ""); kindOf(err) != ErrorInvalidArgument {
		t.Fatalf("empty bundle: %v", err)
	}
}
