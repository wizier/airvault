package engine

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/wizier/airvault/internal/ios"
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
