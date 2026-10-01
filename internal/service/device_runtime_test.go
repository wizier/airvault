package service

import (
	"context"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/engine"
)

// Presence is single-layer: a device is online exactly while the muxer lists it,
// over either transport. netmuxd's heartbeat removes a slept or dropped Wi-Fi
// device from the list.
func TestPresenceFollowsMuxerList(t *testing.T) {
	for _, transport := range []engine.Connection{engine.ConnectionWiFi, engine.ConnectionUSB} {
		t.Run(string(transport), func(t *testing.T) {
			live := newDeviceRuntimeStore()
			presence := map[string]engine.Connection{"phone": transport}
			if transitions := live.applyPresence(presence); len(transitions) != 1 || transitions[0].to != transport {
				t.Fatalf("attach transitions = %#v, want offline→%s", transitions, transport)
			}
			if transitions := live.applyPresence(presence); transitions != nil {
				t.Fatalf("an unchanged muxer list reported transitions %#v", transitions)
			}
			if got := live.connection("phone"); got != transport {
				t.Fatalf("a listed %s phone is online, got %q", transport, got)
			}
			// The muxer delisting it (netmuxd's heartbeat gave up) folds into offline.
			transitions := live.applyPresence(nil)
			if len(transitions) != 1 || transitions[0].from != transport || transitions[0].to != "" {
				t.Fatalf("detach transitions = %#v, want %s→offline", transitions, transport)
			}
			if got := live.connection("phone"); got != "" {
				t.Fatalf("a delisted phone is offline, got %q", got)
			}
		})
	}
}

func TestScreenLockDoesNotGateConnection(t *testing.T) {
	live := newDeviceRuntimeStore()
	presence := map[string]engine.Connection{"phone": engine.ConnectionWiFi}
	live.applyPresence(presence)
	if _, lockScreen, applied := live.applyScreenLock("phone", engine.ScreenLockComplete, time.Now(), screenLockPairWindow); !applied || !lockScreen {
		t.Fatalf("lock signal not applied: applied=%v lockScreen=%v", applied, lockScreen)
	}
	// Screen lock is a UI fact, never a reachability gate.
	if got := live.connection("phone"); got != engine.ConnectionWiFi {
		t.Fatalf("screen lock must not gate connection, got %q", got)
	}
}

// The lock screen shows until an unlock is seen, so the first unlock after a
// (re)connect flips the projection, and a lock's trailing lockstate pulse is
// not an unlock. An unlock seen before a USB hop is forgotten.
func TestScreenLockProjection(t *testing.T) {
	live := newDeviceRuntimeStore()
	live.applyPresence(map[string]engine.Connection{"phone": engine.ConnectionWiFi})
	start := time.Now()
	unlockedAt := func() time.Time { return live.snapshot()["phone"].unlockedAt }

	if changed, lockScreen, _ := live.applyScreenLock("phone", engine.ScreenLockChanged, start, screenLockPairWindow); !changed || lockScreen {
		t.Fatalf("unknown→unlock: changed=%v lockScreen=%v, want a changed unlock", changed, lockScreen)
	}
	if !unlockedAt().Equal(start) {
		t.Fatalf("unlockedAt = %v, want %v", unlockedAt(), start)
	}
	// A repeated lockstate while unlocked keeps the unlock's start.
	if changed, _, _ := live.applyScreenLock("phone", engine.ScreenLockChanged, start.Add(time.Minute), screenLockPairWindow); changed || !unlockedAt().Equal(start) {
		t.Fatalf("repeated unlock: changed=%v since %v, want unchanged since %v", changed, unlockedAt(), start)
	}

	lockAt := start.Add(2 * time.Minute)
	if changed, lockScreen, _ := live.applyScreenLock("phone", engine.ScreenLockComplete, lockAt, screenLockPairWindow); !changed || !lockScreen {
		t.Fatalf("lock: changed=%v lockScreen=%v, want a changed lock screen", changed, lockScreen)
	}
	if changed, lockScreen, _ := live.applyScreenLock("phone", engine.ScreenLockChanged, lockAt.Add(100*time.Millisecond), screenLockPairWindow); changed || !lockScreen {
		t.Fatalf("trailing lock pulse: changed=%v lockScreen=%v, want the unchanged lock screen", changed, lockScreen)
	}

	live.applyScreenLock("phone", engine.ScreenLockChanged, lockAt.Add(time.Minute), screenLockPairWindow)
	live.applyPresence(map[string]engine.Connection{"phone": engine.ConnectionUSB})
	live.applyPresence(map[string]engine.Connection{"phone": engine.ConnectionWiFi})
	if r := live.snapshot()["phone"]; !r.lockScreen() || !r.unlockedAt.IsZero() {
		t.Fatalf("after a USB hop = %+v, want the lock screen", r)
	}
}

// A lockdown read failure must not erase the last known activation state.
func TestApplyActivationKeepsLastKnownOnFailedRead(t *testing.T) {
	store := newDeviceRuntimeStore()
	store.applyPresence(map[string]engine.Connection{"udid-1": engine.ConnectionWiFi})

	if !store.applyActivation("udid-1", "Unactivated") {
		t.Fatal("first real state should register as a change")
	}
	if store.applyActivation("udid-1", "") {
		t.Fatal("failed read must not count as a change")
	}
	if got := store.snapshot()["udid-1"].activation; got != "Unactivated" {
		t.Fatalf("activation = %q, want last known Unactivated", got)
	}
	if !store.applyActivation("udid-1", "Activated") {
		t.Fatal("real transition should register as a change")
	}
}

// The muxer status changes only when what the muxer proved changes; the
// first check and every up/down change are flips.
func TestMuxerStatus(t *testing.T) {
	live := newDeviceRuntimeStore()
	up := engine.PresenceState{MuxUp: true, Devices: []engine.DevicePresence{
		{DeviceID: "a", Connection: "usb"}, {DeviceID: "b", Connection: "wifi"}, {DeviceID: "c", Connection: "wifi"},
	}}
	status, changed, flipped := live.applyMuxer(up)
	if want := (MuxerStatus{Up: true, USB: 1, WiFi: 2}); status != want || !changed || !flipped {
		t.Fatalf("first check = %+v, changed %v, flipped %v", status, changed, flipped)
	}
	if _, changed, _ := live.applyMuxer(up); changed {
		t.Fatal("the same proof changed the status")
	}
	up.Devices = up.Devices[:1]
	if status, changed, flipped := live.applyMuxer(up); !changed || flipped || status.WiFi != 0 {
		t.Fatalf("device change = %+v, changed %v, flipped %v", status, changed, flipped)
	}
	status, changed, flipped = live.applyMuxer(engine.PresenceState{Err: context.DeadlineExceeded})
	if status.Up || !changed || !flipped || status.USB != 0 || status.Error == "" {
		t.Fatalf("down = %+v, changed %v, flipped %v", status, changed, flipped)
	}
	if live.muxerStatus() != status {
		t.Fatal("the stored status differs from the one applied")
	}
}
