package engine

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
)

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
