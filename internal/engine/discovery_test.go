package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/iostest"
)

func TestInspectDevicesReportsPairingAndIdentity(t *testing.T) {
	p := newTestPhone(t)
	p.phone.SetValue("com.apple.mobile.backup", "WillEncrypt", true)
	p.phone.SetValue("", "ActivationState", "Activated")

	// A stranger paired with some other host: AirVault has no record for it.
	_, identity := iostest.NewPairing(t)
	stranger := iostest.NewDevice("STRANGER", identity)
	p.muxer.Attach(stranger, ios.ConnectionNetwork)
	// A phone AirVault has a record for, but which no longer trusts it.
	revoked := iostest.NewDevice("REVOKED", identity)
	p.muxer.Attach(revoked, ios.ConnectionUSB)
	if err := p.engine.pairs.Save("REVOKED", p.record); err != nil {
		t.Fatal(err)
	}
	// A record that does not parse can never open a session.
	p.muxer.Attach(iostest.NewDevice("UNUSABLE", identity), ios.ConnectionUSB)
	if err := os.WriteFile(filepath.Join(p.engine.pairs.root, "UNUSABLE.plist"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	infos, err := p.engine.InspectDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byUDID := map[DeviceID]DeviceInfo{}
	for _, info := range infos {
		byUDID[info.DeviceID] = info
	}
	want := DeviceInfo{
		DeviceID: p.udid, Name: "Test iPhone", ProductType: "iPhone17,1", IOSVersion: "26.0",
		PairingState: PairingStatePaired, MetadataKnown: true, FlagsKnown: true, Encrypted: true,
		ActivationState: "Activated",
	}
	if got := byUDID[p.udid]; got != want {
		t.Errorf("paired phone = %+v\nwant %+v", got, want)
	}
	for _, udid := range []DeviceID{"STRANGER", "REVOKED", "UNUSABLE"} {
		if got := byUDID[udid]; got.PairingState != PairingStateUnpaired || !got.MetadataKnown || got.FlagsKnown {
			t.Errorf("%s = %+v, want unpaired with metadata and unknown flags", udid, got)
		}
	}
}

// A device on both transports is listed once, by USB.
func TestPresenceAndUSBList(t *testing.T) {
	p := newTestPhone(t)
	p.muxer.Attach(p.phone, ios.ConnectionNetwork)
	_, identity := iostest.NewPairing(t)
	p.muxer.Attach(iostest.NewDevice("WIFI-ONLY", identity), ios.ConnectionNetwork)
	ctx := context.Background()

	watcher := p.engine.WatchPresence(ctx)
	defer watcher.Close()
	presence, err := watcher.Next()
	if err != nil {
		t.Fatal(err)
	}
	want := []DevicePresence{{DeviceID: p.udid, Connection: "usb"}, {DeviceID: "WIFI-ONLY", Connection: "wifi"}}
	if !reflect.DeepEqual(presence.Devices, want) {
		t.Fatalf("presence = %+v, want %+v", presence.Devices, want)
	}
	usb, err := p.engine.ListUSBDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []USBDevice{{DeviceID: p.udid, Name: "Test iPhone"}}; !reflect.DeepEqual(usb, want) {
		t.Fatalf("USB devices = %+v, want %+v", usb, want)
	}
}

func TestBattery(t *testing.T) {
	p := newTestPhone(t)
	p.phone.SetValue("com.apple.mobile.battery", "BatteryCurrentCapacity", 87)
	p.phone.SetValue("com.apple.mobile.battery", "BatteryIsCharging", true)
	battery, err := p.engine.Battery(context.Background(), p.udid)
	if err != nil || battery != (Battery{Charging: true, Level: 87}) {
		t.Fatalf("Battery = %+v, %v", battery, err)
	}
	// External power wins over the charging flag: a full phone on a cable.
	p.phone.SetValue("com.apple.mobile.battery", "ExternalConnected", false)
	if battery, _ := p.engine.Battery(context.Background(), p.udid); battery.Charging {
		t.Fatal("ExternalConnected=false must win over BatteryIsCharging")
	}
}

func TestDeviceErrors(t *testing.T) {
	p := newTestPhone(t)
	ctx := context.Background()
	if _, err := p.engine.Battery(ctx, "ABSENT"); kindOf(err) != ErrorDeviceUnavailable {
		t.Errorf("absent device: %v, want ErrorDeviceUnavailable", err)
	}
	if err := p.engine.pairs.Delete(string(p.udid)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.engine.Battery(ctx, p.udid); kindOf(err) != ErrorTrustRequired {
		t.Errorf("no pairing record: %v, want ErrorTrustRequired", err)
	}
	if _, err := p.engine.Battery(ctx, "bad/udid"); kindOf(err) != ErrorInvalidArgument {
		t.Errorf("bad udid: %v, want ErrorInvalidArgument", err)
	}
}
