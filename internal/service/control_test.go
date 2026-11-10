package service

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/iostest"
	"github.com/wizier/airvault/internal/model"
)

// A phone that refuses a power request is online: the failure names the request.
func TestPowerReportsARefusal(t *testing.T) {
	svc, _ := newStoredService(t)
	record, identity := iostest.NewPairing(t)
	phone := iostest.NewDevice("PHONE-UDID", identity)
	phone.Trust(record.HostID)
	phone.Handle(ios.DiagnosticsService, func(raw net.Conn) {
		conn := ios.NewPlistConn(raw, plist.XMLFormat)
		var request map[string]any
		for conn.Recv(&request) == nil && conn.Send(map[string]any{"Status": "Failure"}) == nil {
		}
	})
	muxer := iostest.NewMuxer(t)
	muxer.Attach(phone, ios.ConnectionUSB)

	pairings := t.TempDir()
	pairing, err := record.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pairings, phone.UDID+".plist"), pairing, 0o600); err != nil {
		t.Fatal(err)
	}
	svc.engine, err = engine.New(engine.Config{MuxAddress: muxer.Address(), PairingRoot: pairings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.engine.Close)
	if err := svc.store.Device.Upsert(t.Context(), &model.Device{UDID: phone.UDID, Name: "Phone", Paired: true}); err != nil {
		t.Fatal(err)
	}
	svc.live.applyPresence(map[string]engine.Connection{phone.UDID: engine.ConnectionUSB})

	for action, want := range map[PowerAction]domain.Code{
		PowerRestart: domain.CodePowerRequestFailed,
		PowerSleep:   domain.CodeSleepRequestFailed,
	} {
		err := svc.Power(t.Context(), phone.UDID, action)
		if failure, ok := errors.AsType[*domain.ActionError](err); !ok || failure.Code != want {
			t.Errorf("Power(%s) = %v, want code %q", action, err, want)
		}
	}
}
