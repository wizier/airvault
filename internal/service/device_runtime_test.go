package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/engine"
)

// Presence is single-layer: a device is online exactly while the muxer lists it,
// over either transport. netmuxd's heartbeat removes a slept or dropped Wi-Fi
// device from the list.
func TestPresenceFollowsMuxerList(t *testing.T) {
	for _, transport := range []string{"wifi", "usb"} {
		t.Run(transport, func(t *testing.T) {
			live := newDeviceRuntimeStore()
			presence := map[string]string{"phone": transport}
			live.applyPresence(presence)
			live.publish(presence)
			if got := live.connection("phone"); got != transport {
				t.Fatalf("a listed %s phone is online, got %q", transport, got)
			}
			// The muxer delisting it (netmuxd's heartbeat gave up) folds into offline.
			live.applyPresence(nil)
			transitions := live.publish(nil)
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
	presence := map[string]string{"phone": "wifi"}
	live.applyPresence(presence)
	live.publish(presence)
	changed, locked, applied := live.applyScreenLock("phone", engine.ScreenLockComplete, time.Now(), screenLockPairWindow)
	if !applied || !locked || !changed {
		t.Fatalf("lock signal not applied: applied=%v locked=%v changed=%v", applied, locked, changed)
	}
	// Screen lock is a UI fact, never a reachability gate.
	if got := live.connection("phone"); got != "wifi" {
		t.Fatalf("screen lock must not gate connection, got %q", got)
	}
}

// A lockdown read failure must not erase the last known activation state.
func TestApplyActivationKeepsLastKnownOnFailedRead(t *testing.T) {
	store := newDeviceRuntimeStore()
	store.applyPresence(map[string]string{"udid-1": "wifi"})

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

// fakeEngine drives just OpenLockObserver for the lock-observer supervisor.
type fakeEngine struct {
	openLock func(ctx context.Context, device engine.DeviceID) (engine.LockStream, error)
}

func (f *fakeEngine) OpenLockObserver(ctx context.Context, device engine.DeviceID) (engine.LockStream, error) {
	return f.openLock(ctx, device)
}

type fakeLockStream struct {
	ctx     context.Context
	stopped chan<- struct{}
	once    sync.Once
}

func (s *fakeLockStream) Next() (engine.ScreenLockSignal, error) {
	<-s.ctx.Done()
	s.once.Do(func() {
		if s.stopped != nil {
			s.stopped <- struct{}{}
		}
	})
	return 0, s.ctx.Err()
}

func (s *fakeLockStream) Close() error {
	s.once.Do(func() {
		if s.stopped != nil {
			s.stopped <- struct{}{}
		}
	})
	return nil
}

// The lock observer runs while a device is online and stops when it goes offline.
func TestLockObserverLifecycle(t *testing.T) {
	running := make(chan struct{}, 2)
	stopped := make(chan struct{}, 1)
	fake := &fakeEngine{
		openLock: func(ctx context.Context, _ engine.DeviceID) (engine.LockStream, error) {
			running <- struct{}{}
			return &fakeLockStream{ctx: ctx, stopped: stopped}, nil
		},
	}
	m := newLockObserverMgr(context.Background(), fake, func(string, engine.ScreenLockSignal) {})
	m.setOnline("phone")
	select {
	case <-running:
	case <-time.After(2 * time.Second):
		t.Fatal("online device did not start a lock observer")
	}
	m.setOnline("phone") // idempotent: an already-running observer is not doubled
	m.setOffline("phone")
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("offline device did not stop its lock observer")
	}
	select {
	case <-running:
		t.Fatal("setOnline started a second observer for an already-online device")
	default:
	}
}
