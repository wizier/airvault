package service

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wizier/airvault/internal/engine"
)

// A backup launched for an unlisted phone waits for its next presence signal.
func TestAwaitReachableWakesOnPresence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Service{live: newDeviceRuntimeStore()}
		go func() {
			time.Sleep(10 * time.Millisecond)
			s.live.applyPresence(map[string]engine.Connection{"phone": engine.ConnectionWiFi})
		}()
		if !s.awaitReachable(context.Background(), "phone") {
			t.Fatal("awaitReachable missed the phone coming online")
		}
	})
}

func TestAwaitReachableCancelled(t *testing.T) {
	s := &Service{live: newDeviceRuntimeStore()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.awaitReachable(ctx, "phone") {
		t.Fatal("a cancelled wait reported the phone reachable")
	}
}
