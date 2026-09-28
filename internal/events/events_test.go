package events

import "testing"

// Transient events are neither replayed nor worth a subscriber: one too slow
// for them keeps its stream, one too slow for a lasting event loses it.
func TestBusReplaysAndDropsOnlyWhatItMay(t *testing.T) {
	bus := New()
	bus.Emit(Event{Type: "device.added"})
	bus.Emit(Event{Type: "backup.progress", Transient: true})
	_, live, replay, complete := bus.Subscribe(1)
	if !complete || len(replay) != 0 {
		t.Fatalf("replay after 1 = %v, complete %v; want nothing", replay, complete)
	}

	for range cap(live) + 1 {
		bus.Emit(Event{Type: "backup.progress", Transient: true})
	}
	if _, open := <-live; !open {
		t.Fatal("a dropped transient event closed the subscriber")
	}
	for range cap(live) + 1 {
		bus.Emit(Event{Type: "device.updated"})
	}
	for range live {
	}

	_, _, replay, complete = bus.Subscribe(1)
	if !complete || len(replay) != cap(live)+1 {
		t.Fatalf("replay = %d events, complete %v; want the %d lasting ones", len(replay), complete, cap(live)+1)
	}
}
