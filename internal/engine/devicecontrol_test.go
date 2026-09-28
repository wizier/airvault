package engine

import (
	"bytes"
	"context"
	"reflect"
	"sync"
	"testing"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
)

func TestHardwareReport(t *testing.T) {
	p := newTestPhone(t)
	p.phone.SetValue("", "SerialNumber", "F2LXXXX")
	p.phone.SetValue("", "CarrierBundleInfoArray", []any{map[string]any{"CFBundleIdentifier": "com.apple.carrier", "Slot": "kOne"}})
	p.phone.SetValue("com.apple.disk_usage", "TotalDataCapacity", uint64(128<<30))
	p.phone.SetValue("com.apple.fmip", "IsAssociated", true)
	p.phone.Handle(ios.DiagnosticsService, xmlService(func(request map[string]any) map[string]any {
		if request["Request"] != "IORegistry" || request["EntryName"] != "AppleSmartBattery" {
			return map[string]any{"Status": "Failure"}
		}
		return map[string]any{"Status": "Success", "Diagnostics": map[string]any{
			"IORegistry": map[string]any{"CycleCount": 312, "Maximum Capacity Percent": 91, "InstantAmperage": -420},
		}}
	}))

	report, err := p.engine.HardwareReport(context.Background(), p.udid)
	if err != nil {
		t.Fatal(err)
	}
	if report.Lockdown.SerialNumber != "F2LXXXX" || report.Lockdown.ProductType != "iPhone17,1" ||
		!reflect.DeepEqual(report.Lockdown.Carriers, []CarrierBundle{{Bundle: "com.apple.carrier", Slot: "kOne"}}) {
		t.Errorf("lockdown = %+v", report.Lockdown)
	}
	if report.DiskUsage.TotalDataCapacity != 128<<30 {
		t.Errorf("disk usage = %+v", report.DiskUsage)
	}
	if report.FindMy == nil || !*report.FindMy {
		t.Errorf("find my = %v", report.FindMy)
	}
	if want := (BatteryGauge{CycleCount: 312, MaximumCapacityPercentWithSpaces: 91, InstantAmperage: -420}); report.Battery != want {
		t.Errorf("battery = %+v, want %+v", report.Battery, want)
	}
}

// Only the session is required: parts the phone does not provide stay zero.
func TestHardwareReportIsPartial(t *testing.T) {
	p := newTestPhone(t)
	report, err := p.engine.HardwareReport(context.Background(), p.udid)
	if err != nil {
		t.Fatal(err)
	}
	if report.FindMy != nil || report.Battery != (BatteryGauge{}) || report.Lockdown.ProductType != "iPhone17,1" {
		t.Fatalf("partial report = %+v", report)
	}
}

func TestPower(t *testing.T) {
	p := newTestPhone(t)
	var mu sync.Mutex
	var received []any
	p.phone.Handle(ios.DiagnosticsService, xmlService(func(request map[string]any) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, request["Request"])
		return map[string]any{"Status": "Success"}
	}))
	ctx := context.Background()
	for _, action := range []PowerAction{PowerRestart, PowerShutdown, PowerSleep} {
		if err := p.engine.Power(ctx, p.udid, action); err != nil {
			t.Fatal(err)
		}
	}
	if want := []any{"Restart", "Shutdown", "Sleep"}; !reflect.DeepEqual(received, want) {
		t.Fatalf("requests = %v, want %v", received, want)
	}
	if err := p.engine.Power(ctx, p.udid, PowerSleep+1); kindOf(err) != ErrorInvalidArgument {
		t.Fatalf("bad action: %v", err)
	}
}

func TestListApps(t *testing.T) {
	p := newTestPhone(t)
	p.phone.Handle(ios.InstallationProxyService, xmlService(func(request map[string]any) map[string]any {
		if options, _ := request["ClientOptions"].(map[string]any); request["Command"] != "Lookup" || options["ApplicationType"] != "User" {
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

func TestWallpaper(t *testing.T) {
	p := newTestPhone(t)
	p.phone.Handle(ios.SpringBoardService, xmlService(func(request map[string]any) map[string]any {
		return map[string]any{"pngData": []byte(request["wallpaperName"].(string))}
	}))
	for lockScreen, want := range map[bool]string{true: "lockscreen", false: "homescreen"} {
		if png, err := p.engine.Wallpaper(context.Background(), p.udid, lockScreen); err != nil || string(png) != want {
			t.Fatalf("Wallpaper(%v) = %q, %v", lockScreen, png, err)
		}
	}
}

func TestActivation(t *testing.T) {
	p := newTestPhone(t)
	var mu sync.Mutex
	var finished map[string]any
	daemonUp := true
	p.phone.Handle(ios.ActivationService, xmlService(func(request map[string]any) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		switch request["Command"] {
		case "GetActivationStateRequest":
			if !daemonUp {
				return map[string]any{"Error": "NotReady"}
			}
			return map[string]any{"Value": "Unactivated"}
		case "CreateTunnel1SessionInfoRequest":
			return map[string]any{"Value": map[string]any{"HandshakeRequestMessage": []byte("hello")}}
		case "CreateTunnel1ActivationInfoRequest":
			return map[string]any{"Value": map[string]any{"Echo": request["Value"]}}
		case "HandleActivationInfoWithSessionRequest":
			if string(request["Value"].([]byte)) == "rejected" {
				return map[string]any{"Error": "InvalidActivationRecord"}
			}
			finished = request
			return map[string]any{} // the daemon accepts without a Value
		}
		return map[string]any{"Error": "UnknownCommand"}
	}))
	ctx := context.Background()

	if state, err := p.engine.ActivationState(ctx, p.udid); err != nil || state != "Unactivated" {
		t.Fatalf("state from the daemon = %q, %v", state, err)
	}
	mu.Lock()
	daemonUp = false
	mu.Unlock()
	p.phone.SetValue("", "ActivationState", "FactoryActivated")
	if state, err := p.engine.ActivationState(ctx, p.udid); err != nil || state != "FactoryActivated" {
		t.Fatalf("state from lockdown = %q, %v", state, err)
	}

	info, err := p.engine.ActivationSessionInfo(ctx, p.udid)
	if err != nil || !bytes.Contains(info, []byte("<key>HandshakeRequestMessage</key>")) {
		t.Fatalf("session info = %s, %v", info, err)
	}
	signed, err := p.engine.ActivationInfo(ctx, p.udid, []byte("handshake"))
	if err != nil {
		t.Fatal(err)
	}
	var echoed struct {
		Echo []byte `plist:"Echo"`
	}
	if _, err := plist.Unmarshal(signed, &echoed); err != nil || string(echoed.Echo) != "handshake" {
		t.Fatalf("activation info = %s, %v", signed, err)
	}
	if _, err := p.engine.ActivationInfo(ctx, p.udid, nil); kindOf(err) != ErrorInvalidArgument {
		t.Fatalf("empty handshake: %v", err)
	}

	if err := p.engine.ActivationFinish(ctx, p.udid, []byte("rejected"), nil); err == nil {
		t.Fatal("a record the daemon refused was applied")
	}
	if err := p.engine.ActivationFinish(ctx, p.udid, []byte("record"), map[string]string{"X-Sig": "1"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if string(finished["Value"].([]byte)) != "record" || finished["ActivationResponseHeaders"].(map[string]any)["X-Sig"] != "1" {
		t.Fatalf("finish request = %v", finished)
	}
	if p.phone.Value("", "ActivationStateAcknowledged") != true {
		t.Fatal("activation was not acknowledged over lockdown")
	}
}
