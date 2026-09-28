package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/iostest"
)

// newUnpairedPhone is a USB phone AirVault holds no record for.
func newUnpairedPhone(t *testing.T) *testPhone {
	t.Helper()
	_, identity := iostest.NewPairing(t)
	phone := iostest.NewDevice("NEW-PHONE", identity)
	phone.SetValue("", "DevicePublicKey", iostest.DevicePublicKey(t))
	phone.SetValue("", "WiFiAddress", "aa:bb:cc:dd:ee:ff")
	muxer := iostest.NewMuxer(t)
	muxer.Attach(phone, ios.ConnectionUSB)
	engine, err := New(Config{MuxAddress: muxer.Address(), PairingRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return &testPhone{engine: engine, muxer: muxer, phone: phone, udid: DeviceID(phone.UDID)}
}

func advance(t *testing.T, p *testPhone, want PairingOutcome) {
	t.Helper()
	if outcome, err := p.engine.AdvancePairing(context.Background(), p.udid); err != nil || outcome != want {
		t.Fatalf("AdvancePairing = %q, %v; want %q", outcome, err, want)
	}
}

func TestPairingNewDevice(t *testing.T) {
	p := newUnpairedPhone(t)
	p.phone.AnswerPairing("PairingDialogResponsePending")
	advance(t, p, TrustPending)
	first, _ := p.phone.Offered()["HostID"].(string)
	advance(t, p, TrustPending)
	if again, _ := p.phone.Offered()["HostID"].(string); first == "" || again != first {
		t.Fatalf("offered HostIDs %q then %q: a pending pairing must keep its identity", first, again)
	}
	for key := range p.phone.Offered() {
		if key == "HostPrivateKey" || key == "RootPrivateKey" {
			t.Fatalf("the Pair request sent %s to the phone", key)
		}
	}

	p.phone.AnswerPairing("")
	advance(t, p, TrustPaired)
	record, err := p.engine.pairs.Load(string(p.udid))
	if err != nil || record == nil || record.HostID != first || string(record.EscrowBag) != "IOSTEST-ESCROW" {
		t.Fatalf("saved record = %+v, %v", record, err)
	}
	if identity, _ := p.engine.pairs.Identity(string(p.udid)); identity != nil {
		t.Fatal("the pending identity outlived the pairing")
	}
	if p.muxer.Record(string(p.udid)) == nil {
		t.Fatal("the muxer got no record, so it cannot find the phone on Wi-Fi")
	}
	if p.phone.Value("com.apple.mobile.wireless_lockdown", "EnableWifiConnections") != true {
		t.Fatal("Wi-Fi connections were not enabled")
	}
	// The generated record really authenticates.
	p.phone.SetValue("com.apple.mobile.battery", "BatteryCurrentCapacity", 50)
	if _, err := p.engine.Battery(context.Background(), p.udid); err != nil {
		t.Fatalf("session with the new record: %v", err)
	}
	// Paired already: the next step finishes without asking again.
	p.phone.AnswerPairing("UserDeniedPairing")
	advance(t, p, TrustPaired)
}

func TestPairingAnswers(t *testing.T) {
	p := newUnpairedPhone(t)
	for answer, want := range map[string]PairingOutcome{
		"UserDeniedPairing": TrustDenied,
		"PasswordProtected": TrustLocked,
	} {
		p.phone.AnswerPairing(answer)
		advance(t, p, want)
	}
}

// The phone forgot this host: pairing starts over and replaces the record.
func TestPairingAfterTheDeviceForgot(t *testing.T) {
	p := newTestPhone(t)
	p.phone.SetValue("", "DevicePublicKey", iostest.DevicePublicKey(t))
	p.phone.SetValue("", "WiFiAddress", "aa:bb:cc:dd:ee:ff")
	stranger := *p.record
	stranger.HostID = "FORGOTTEN-HOST"
	if err := p.engine.pairs.Save(string(p.udid), &stranger); err != nil {
		t.Fatal(err)
	}
	advance(t, p, TrustPaired)
	record, err := p.engine.pairs.Load(string(p.udid))
	if err != nil || record == nil || record.HostID == "FORGOTTEN-HOST" || !p.phone.Trusts(record.HostID) {
		t.Fatalf("record after re-pairing = %+v, %v", record, err)
	}
}

// An unusable record does not block pairing: the new record replaces it.
func TestPairingReplacesAnUnusableRecord(t *testing.T) {
	p := newUnpairedPhone(t)
	path := filepath.Join(p.engine.pairs.root, string(p.udid)+".plist")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	advance(t, p, TrustPaired)
	if record, err := p.engine.pairs.Load(string(p.udid)); record == nil || err != nil {
		t.Fatalf("record after pairing = %v, %v", record, err)
	}
}

// A record the muxer already has — maybe Finder's — is left alone.
func TestPairingKeepsTheMuxerRecord(t *testing.T) {
	p := newTestPhone(t)
	finder := []byte("Finder's record")
	mux, _ := ios.ParseMux(p.muxer.Address())
	if err := mux.SavePairRecord(context.Background(), string(p.udid), finder); err != nil {
		t.Fatal(err)
	}
	advance(t, p, TrustPaired)
	if string(p.muxer.Record(string(p.udid))) != string(finder) {
		t.Fatal("the muxer's record was replaced")
	}
}

func TestUnpair(t *testing.T) {
	p := newTestPhone(t)
	if _, err := p.engine.pairs.ReserveIdentity(string(p.udid), pairIdentity{HostID: "H", SystemBUID: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := p.engine.Unpair(context.Background(), p.udid); err != nil {
		t.Fatal(err)
	}
	if p.phone.Trusts(p.record.HostID) {
		t.Fatal("the device still trusts this host")
	}
	if record, _ := p.engine.pairs.Load(string(p.udid)); record != nil {
		t.Fatal("the record survived Unpair")
	}
	if identity, _ := p.engine.pairs.Identity(string(p.udid)); identity != nil {
		t.Fatal("the pending identity survived Unpair")
	}
	// Nothing left, or no device at all: host state still goes, without error.
	if err := p.engine.Unpair(context.Background(), p.udid); err != nil {
		t.Fatal(err)
	}
	if err := p.engine.Unpair(context.Background(), "ABSENT"); err != nil {
		t.Fatal(err)
	}
	if err := p.engine.Unpair(context.Background(), "bad/udid"); kindOf(err) != ErrorInvalidArgument {
		t.Fatalf("bad udid: %v", err)
	}
}
