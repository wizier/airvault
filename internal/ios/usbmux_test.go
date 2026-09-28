package ios

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"
)

func TestParseMux(t *testing.T) {
	tests := map[string]Mux{
		"":                {"unix", "/var/run/usbmuxd"},
		"/run/netmuxd":    {"unix", "/run/netmuxd"},
		"127.0.0.1:27015": {"tcp", "127.0.0.1:27015"},
		"localhost:27015": {"tcp", "localhost:27015"},
		"[::1]:27015":     {"tcp", "[::1]:27015"},
		"muxer.lan:27015": {"tcp", "muxer.lan:27015"},
	}
	for address, want := range tests {
		if got, err := ParseMux(address); err != nil || got != want {
			t.Errorf("ParseMux(%q) = %v, %v; want %v", address, got, err, want)
		}
	}
	for _, address := range []string{":27015", "host:port", "host:70000", "a:b:c"} {
		if _, err := ParseMux(address); err == nil {
			t.Errorf("ParseMux(%q) accepted", address)
		}
	}
}

func TestDevicesDecodesAttachments(t *testing.T) {
	bsd := []byte{0x10, 2, 0xf2, 0x7e, 192, 168, 1, 20, 0, 0, 0, 0, 0, 0, 0, 0}
	linux := make([]byte, 28)
	linux[0] = 10
	copy(linux[8:], netip.MustParseAddr("fe80::1").AsSlice())
	mux := startMux(t, func(conn net.Conn, request map[string]any) {
		defer conn.Close()
		if request["MessageType"] != "ListDevices" || request["ClientVersionString"] != "AirVault" {
			t.Errorf("request = %v", request)
		}
		_ = writeMux(conn, map[string]any{"DeviceList": []any{
			device(1, "PHONE", "USB", nil),
			device(2, "PHONE", "Network", bsd),
			device(3, "PAD", "Network", linux),
		}})
	})
	devices, err := mux.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Device{
		{ID: 1, UDID: "PHONE", Connection: ConnectionUSB},
		{ID: 2, UDID: "PHONE", Connection: ConnectionNetwork, Addr: netip.MustParseAddr("192.168.1.20")},
		{ID: 3, UDID: "PAD", Connection: ConnectionNetwork, Addr: netip.MustParseAddr("fe80::1")},
	}
	if !reflect.DeepEqual(devices, want) {
		t.Fatalf("devices = %+v\nwant %+v", devices, want)
	}
}

func device(id uint64, udid, connection string, address []byte) map[string]any {
	properties := map[string]any{"ConnectionType": connection, "SerialNumber": udid}
	if address != nil {
		properties["NetworkAddress"] = address
	}
	return map[string]any{"DeviceID": id, "MessageType": "Attached", "Properties": properties}
}

func TestPreferUSBKeepsOneEntryPerDevice(t *testing.T) {
	wifi := Device{ID: 1, UDID: "A", Connection: ConnectionNetwork}
	usb := Device{ID: 2, UDID: "A", Connection: ConnectionUSB}
	other := Device{ID: 3, UDID: "B", Connection: ConnectionNetwork}
	got := PreferUSB([]Device{wifi, other, usb, {ID: 4, UDID: "A", Connection: ConnectionNetwork}})
	if want := []Device{usb, other}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PreferUSB = %+v, want %+v", got, want)
	}
}

// The port travels byte-swapped; a refused port is a MuxError.
func TestConnect(t *testing.T) {
	mux := startMux(t, func(conn net.Conn, request map[string]any) {
		switch request["PortNumber"] {
		case uint64(32498): // 62078 in network order
			muxResult(conn, 0)
			_, _ = conn.Write([]byte("device"))
		default:
			muxResult(conn, uint64(MuxConnectionRefused))
		}
		_ = conn.Close()
	})
	conn, err := mux.Connect(context.Background(), Device{ID: 7}, LockdownPort)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 6)
	if _, err := conn.Read(buffer); err != nil || string(buffer) != "device" {
		t.Fatalf("stream after connect: %q, %v", buffer, err)
	}
	_ = conn.Close()
	if _, err := mux.Connect(context.Background(), Device{ID: 7}, 1); !errors.Is(err, MuxConnectionRefused) {
		t.Fatalf("refused port: %v, want MuxConnectionRefused", err)
	}
}

func TestPairRecordRequests(t *testing.T) {
	var saved map[string]any
	mux := startMux(t, func(conn net.Conn, request map[string]any) {
		defer conn.Close()
		switch request["MessageType"] {
		case "ReadBUID":
			_ = writeMux(conn, map[string]any{"BUID": "BUID-1"})
		case "ReadPairRecord":
			if request["PairRecordID"] == "KNOWN" {
				_ = writeMux(conn, map[string]any{"PairRecordData": []byte("record")})
				return
			}
			muxResult(conn, uint64(MuxBadDevice))
		case "SavePairRecord":
			saved = request
			muxResult(conn, 0)
		}
	})
	ctx := context.Background()
	if buid, err := mux.BUID(ctx); err != nil || buid != "BUID-1" {
		t.Fatalf("BUID = %q, %v", buid, err)
	}
	if data, err := mux.PairRecord(ctx, "KNOWN"); err != nil || string(data) != "record" {
		t.Fatalf("PairRecord = %q, %v", data, err)
	}
	if _, err := mux.PairRecord(ctx, "OTHER"); !errors.Is(err, MuxBadDevice) {
		t.Fatalf("missing record: %v, want MuxBadDevice", err)
	}
	if err := mux.SavePairRecord(ctx, "KNOWN", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if saved["PairRecordID"] != "KNOWN" || string(saved["PairRecordData"].([]byte)) != "new" {
		t.Fatalf("SavePairRecord sent %v", saved)
	}
}

func TestListen(t *testing.T) {
	mux := startMux(t, func(conn net.Conn, request map[string]any) {
		muxResult(conn, 0)
		_ = writeMux(conn, map[string]any{"MessageType": "Attached", "DeviceID": uint64(5)})
		_ = writeMux(conn, map[string]any{"MessageType": "Detached", "DeviceID": uint64(5)})
		_ = conn.Close()
	})
	listener, err := mux.Listen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, want := range []ListenEvent{{"Attached", 5}, {"Detached", 5}} {
		if got, err := listener.Next(context.Background()); err != nil || got != want {
			t.Fatalf("Next = %+v, %v; want %+v", got, err, want)
		}
	}
	if _, err := listener.Next(context.Background()); err == nil {
		t.Fatal("Next after the muxer closed succeeded")
	}
}

// startMux serves a fake muxer on loopback TCP: each accepted connection's
// first request goes to handle, which answers on conn (and may keep it).
func startMux(t *testing.T, handle func(conn net.Conn, request map[string]any)) Mux {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				var request map[string]any
				if err := readMux(conn, &request); err != nil {
					_ = conn.Close()
					return
				}
				handle(conn, request)
			}()
		}
	}()
	return Mux{"tcp", listener.Addr().String()}
}

func muxResult(conn net.Conn, number uint64) {
	_ = writeMux(conn, map[string]any{"MessageType": "Result", "Number": number})
}
