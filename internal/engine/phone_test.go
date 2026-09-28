package engine

import (
	"net"
	"testing"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/iostest"
	"github.com/wizier/airvault/internal/objectstore"
)

// testPhone is a Go engine wired to a fake muxer with one phone paired with
// AirVault and attached by USB.
type testPhone struct {
	engine  *Engine
	muxer   *iostest.Muxer
	phone   *iostest.Device
	record  *ios.PairRecord
	udid    DeviceID
	objects *objectstore.Store
}

func newTestPhone(t *testing.T) *testPhone {
	t.Helper()
	record, identity := iostest.NewPairing(t)
	phone := iostest.NewDevice("PHONE-UDID", identity)
	phone.Trust(record.HostID)
	muxer := iostest.NewMuxer(t)
	muxer.Attach(phone, ios.ConnectionUSB)
	objects, err := objectstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Close() })
	engine, err := New(Config{MuxAddress: muxer.Address(), PairingRoot: t.TempDir(), Objects: objects})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.pairs.Save(phone.UDID, record); err != nil {
		t.Fatal(err)
	}
	return &testPhone{engine: engine, muxer: muxer, phone: phone, record: record, udid: DeviceID(phone.UDID), objects: objects}
}

// servePlist answers each request of a plist service with reply's result;
// a nil reply ends the connection.
func servePlist(format int, reply func(request map[string]any) map[string]any) iostest.Handler {
	return func(raw net.Conn) {
		conn := ios.NewPlistConn(raw, format)
		for {
			var request map[string]any
			if err := conn.Recv(&request); err != nil {
				return
			}
			answer := reply(request)
			if answer == nil {
				return
			}
			if err := conn.Send(answer); err != nil {
				return
			}
		}
	}
}

func xmlService(reply func(request map[string]any) map[string]any) iostest.Handler {
	return servePlist(plist.XMLFormat, reply)
}

func kindOf(err error) ErrorKind {
	if engineErr, ok := err.(*Error); ok {
		return engineErr.Kind
	}
	return 0
}
