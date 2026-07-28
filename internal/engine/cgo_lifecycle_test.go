package engine

import (
	"slices"
	"testing"
	"time"
)

func TestNativeHandleCloseCancelsBeforeDestroying(t *testing.T) {
	handle := newNativeHandle(7)
	entered := make(chan struct{})
	releaseCall := make(chan struct{})
	go func() {
		value, leave, ok := handle.enter()
		if !ok || value != 7 {
			t.Errorf("enter = (%d, %v), want (7, true)", value, ok)
			return
		}
		close(entered)
		<-releaseCall
		leave()
	}()
	<-entered

	cancelled := make(chan struct{})
	destroyed := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		handle.close(
			func(value int) { close(cancelled) },
			func(value int) { close(destroyed) },
		)
		close(closed)
	}()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Close did not signal cancellation")
	}
	select {
	case <-destroyed:
		t.Fatal("Close destroyed a handle with an active FFI call")
	default:
	}

	close(releaseCall)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after the active call returned")
	}
}

func TestNativeHandleDetachTransfersOwnership(t *testing.T) {
	handle := newNativeHandle(7)
	value, leave, ok := handle.enter()
	if !ok || !handle.detach(value) {
		t.Fatal("active call could not detach its handle")
	}
	leave()

	destroyed := false
	handle.close(nil, func(int) { destroyed = true })
	if destroyed {
		t.Fatal("Close destroyed a handle owned by the completed transition")
	}
}

func TestLatestDispatcherCoalescesAndFlushes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var received []int
	dispatcher := newLatestDispatcher("test", func(value int) {
		if value == 1 {
			close(started)
			<-release
		}
		received = append(received, value)
	})
	dispatcher.submit(1)
	<-started
	dispatcher.submit(2)
	dispatcher.submit(3)
	close(release)
	dispatcher.close()

	if !slices.Equal(received, []int{1, 3}) {
		t.Fatalf("received = %v, want [1 3]", received)
	}
}
