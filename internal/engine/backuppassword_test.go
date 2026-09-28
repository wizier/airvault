package engine

import (
	"context"
	"testing"

	"github.com/wizier/airvault/internal/ios/backup2"
	"github.com/wizier/airvault/internal/ios/iostest"
)

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
