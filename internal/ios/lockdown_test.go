package ios_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/iostest"
)

func startPhone(t *testing.T) (ios.Mux, ios.Device, *ios.PairRecord, *iostest.Device) {
	t.Helper()
	record, identity := iostest.NewPairing(t)
	phone := iostest.NewDevice("PHONE", identity)
	phone.Trust(record.HostID)
	phone.Handle("com.example.echo", func(conn net.Conn) { _, _ = io.Copy(conn, conn) })
	muxer := iostest.NewMuxer(t)
	id := muxer.Attach(phone, ios.ConnectionUSB)
	mux, err := ios.ParseMux(muxer.Address())
	if err != nil {
		t.Fatal(err)
	}
	return mux, ios.Device{ID: id, UDID: phone.UDID, Connection: ios.ConnectionUSB}, record, phone
}

func TestLockdownSessionAndService(t *testing.T) {
	mux, device, record, phone := startPhone(t)
	ctx := context.Background()

	lockdown, err := ios.DialLockdown(ctx, mux, device)
	if err != nil {
		t.Fatal(err)
	}
	defer lockdown.Close()
	if kind, err := lockdown.QueryType(ctx); err != nil || kind != "com.apple.mobile.lockdown" {
		t.Fatalf("QueryType = %q, %v", kind, err)
	}
	if name, err := lockdown.Value[string](ctx, "", "DeviceName"); err != nil || name != "Test iPhone" {
		t.Fatalf("DeviceName = %q, %v", name, err)
	}
	if _, err := lockdown.StartService(ctx, "com.example.echo", nil); !errors.Is(err, ios.ErrSessionInactive) {
		t.Fatalf("service before a session: %v, want ErrSessionInactive", err)
	}

	if err := lockdown.StartSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := lockdown.SetValue(ctx, "com.apple.mobile.wireless_lockdown", "EnableWifiConnections", true); err != nil {
		t.Fatal(err)
	}
	if phone.Value("com.apple.mobile.wireless_lockdown", "EnableWifiConnections") != true {
		t.Fatal("SetValue did not reach the device")
	}
	if domain, err := lockdown.Value[map[string]any](ctx, "", ""); err != nil || domain["ProductType"] != "iPhone17,1" {
		t.Fatalf("root domain over TLS = %v, %v", domain, err)
	}
	service, err := lockdown.StartService(ctx, "com.example.echo", record.EscrowBag)
	if err != nil {
		t.Fatal(err)
	}
	if service.Name != "com.example.echo" || service.Port == 0 || !service.SSL {
		t.Fatalf("service = %+v", service)
	}
	conn, err := ios.DialService(ctx, mux, device, service, record)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	echo := make([]byte, 4)
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("service echo = %q, %v", echo, err)
	}
}

func TestLockdownRejectsUnknownHost(t *testing.T) {
	mux, device, record, _ := startPhone(t)
	ctx := context.Background()
	lockdown, err := ios.DialLockdown(ctx, mux, device)
	if err != nil {
		t.Fatal(err)
	}
	defer lockdown.Close()
	stranger := *record
	stranger.HostID = "OTHER-HOST"
	if err := lockdown.StartSession(ctx, &stranger); !errors.Is(err, ios.ErrInvalidHostID) {
		t.Fatalf("unknown host: %v, want ErrInvalidHostID", err)
	}
	if _, err := lockdown.Value[string](ctx, "", "Missing"); err == nil {
		t.Fatal("reading a missing value succeeded")
	}
}
