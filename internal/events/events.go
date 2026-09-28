// Package events is the bus behind the SSE stream: it numbers events and keeps
// the lasting ones for a reconnecting client to replay.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// StreamReset tells a client its replay is incomplete: it reloads instead.
const StreamReset = "stream.reset"

// Event is one server-sent event. A transient one, like a progress frame, is
// superseded by the next: it is not kept for replay, and a subscriber too slow
// for it just misses it.
type Event struct {
	ID        uint64
	Type      string
	Data      any
	Transient bool
}

type Bus struct {
	epoch   string
	mu      sync.Mutex
	subs    map[int]chan Event
	nextSub int
	nextID  uint64
	floor   uint64
	history []Event
}

const historyLimit = 256

// The random epoch scopes event IDs to this process: a Last-Event-ID from
// another epoch cannot be replayed and forces a client resync.
func New() *Bus {
	epoch := make([]byte, 4)
	_, _ = rand.Read(epoch)
	return &Bus{epoch: hex.EncodeToString(epoch), subs: map[int]chan Event{}}
}

func (b *Bus) Epoch() string { return b.epoch }

// Subscribe returns live events plus retained lifecycle events after afterID;
// the bool is false when the caller must reload authoritative snapshots.
func (b *Bus) Subscribe(afterID uint64) (int, <-chan Event, []Event, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSub
	b.nextSub++
	ch := make(chan Event, 64)
	b.subs[id] = ch
	replay := make([]Event, 0, len(b.history))
	if afterID > 0 {
		for _, event := range b.history {
			if event.ID > afterID {
				replay = append(replay, event)
			}
		}
	}
	complete := afterID == 0 || (afterID >= b.floor && afterID <= b.nextID)
	return id, ch, replay, complete
}

func (b *Bus) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// Emit never blocks producers. A subscriber that cannot take a lasting event
// is closed, so it reconnects and replays or resyncs rather than silently
// losing it.
func (b *Bus) Emit(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	e.ID = b.nextID
	if !e.Transient {
		b.history = append(b.history, e)
		if len(b.history) > historyLimit {
			b.floor = b.history[0].ID
			b.history = b.history[1:]
		}
	}
	for id, ch := range b.subs {
		select {
		case ch <- e:
			continue
		default:
		}
		if e.Transient {
			continue
		}
		delete(b.subs, id)
		close(ch)
	}
}
