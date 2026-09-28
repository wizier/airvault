package engine

import (
	"context"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/iostest"
)

// The watcher publishes the muxer's state, each attach and detach, one down
// state while the muxer is gone, and the state again once it is back.
func TestPresenceWatcher(t *testing.T) {
	p := newTestPhone(t)
	watcher := p.engine.WatchPresence(context.Background())
	defer watcher.Close()
	next := func() PresenceState {
		t.Helper()
		state, err := watcher.Next()
		if err != nil {
			t.Fatal(err)
		}
		return state
	}

	if state := next(); !state.MuxUp || len(state.Devices) != 1 {
		t.Fatalf("first state = %+v", state)
	}
	_, identity := iostest.NewPairing(t)
	second := p.muxer.Attach(iostest.NewDevice("SECOND", identity), ios.ConnectionNetwork)
	if state := next(); len(state.Devices) != 2 {
		t.Fatalf("after attach = %+v", state)
	}
	p.muxer.Detach(second)
	if state := next(); len(state.Devices) != 1 {
		t.Fatalf("after detach = %+v", state)
	}
	p.muxer.SetDown(true)
	if state := next(); state.MuxUp || len(state.Devices) != 0 {
		t.Fatalf("muxer down = %+v", state)
	}
	p.muxer.SetDown(false)
	if state := next(); !state.MuxUp || len(state.Devices) != 1 {
		t.Fatalf("muxer back = %+v", state)
	}
}

// A muxer that stops answering is lost once it has been quiet for a
// heartbeat, with the reason, and found again once it answers.
func TestPresenceWatcherNoticesAHungMuxer(t *testing.T) {
	p := newTestPhone(t)
	watcher := p.engine.watchPresence(context.Background(), 50*time.Millisecond, 200*time.Millisecond)
	defer watcher.Close()
	if state, err := watcher.Next(); err != nil || !state.MuxUp {
		t.Fatalf("first state = %+v, %v", state, err)
	}
	p.muxer.Hang(true)
	state, err := watcher.Next()
	if err != nil || state.MuxUp || state.Err == nil {
		t.Fatalf("hung muxer = %+v, %v", state, err)
	}
	p.muxer.Hang(false)
	if state, err := watcher.Next(); err != nil || !state.MuxUp || len(state.Devices) != 1 {
		t.Fatalf("muxer answering again = %+v, %v", state, err)
	}
}
