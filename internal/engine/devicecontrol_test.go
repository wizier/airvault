package engine

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/backup2"
	"github.com/wizier/airvault/internal/ios/iostest"
)

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

// The phone answers an erase or hangs up to start it; with Find My on it is
// never asked, since the erased phone would stay locked to its owner.
func TestEraseDevice(t *testing.T) {
	cases := []struct {
		name   string
		findMy bool
		device func(dl *iostest.DeviceLink)
		kind   ErrorKind
	}{
		{"accepted", false, func(dl *iostest.DeviceLink) { dl.Finish(0, "") }, 0},
		{"hung up to erase", false, func(dl *iostest.DeviceLink) {}, 0},
		{"refused", false, func(dl *iostest.DeviceLink) { dl.Finish(1, "failed") }, ErrorProtocol},
		{"find my on", true, nil, ErrorFindMyEnabled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newTestPhone(t)
			p.phone.SetValue("com.apple.fmip", "IsAssociated", c.findMy)
			var asked atomic.Bool
			p.phone.Handle(backup2.Service, iostest.Backup2(t, func(dl *iostest.DeviceLink) {
				asked.Store(true)
				if request := dl.Request(); request["MessageName"] != "EraseDevice" || request["TargetIdentifier"] != string(p.udid) {
					t.Errorf("request = %v", request)
				}
				c.device(dl)
			}))
			if err := p.engine.EraseDevice(context.Background(), p.udid); kindOf(err) != c.kind {
				t.Fatalf("err = %v, want kind %d", err, c.kind)
			}
			if asked.Load() == c.findMy {
				t.Fatalf("erase asked = %v with Find My %v", asked.Load(), c.findMy)
			}
		})
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
