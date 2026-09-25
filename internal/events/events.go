// Package events is an in-memory pub/sub bus: services publish domain events,
// the SSE handler (GET /api/events) subscribes one channel per browser.
// Publishing is non-blocking — a slow subscriber drops events, never stalls the publisher.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// Event type constants.
const (
	DeviceAdded        = "device.added"
	DeviceUpdated      = "device.updated"
	DeviceRemoved      = "device.removed"
	DeviceOnline       = "device.online"
	DeviceOffline      = "device.offline"
	BackupStarted      = "backup.started"
	BackupProgress     = "backup.progress"
	BackupDone         = "backup.completed"
	BackupFailed       = "backup.failed"
	BackupCancelled    = "backup.cancelled"
	BackupCatalog      = "backup.catalog"
	AppCatalog         = "app.catalog"
	AppInstallProgress = "app.install.progress" // {installId, phase, percent}; droppable
	PairChanged        = "pair.changed"
	PairTrust          = "pair.trust"    // trust-flow progress: {"udid","status","errorCode"?}
	MuxerChanged       = "muxer.changed" // muxer went up or down, {"up":bool}
	StreamReset        = "stream.reset"
)

// Event is one published event. Data is JSON-encoded by the SSE handler.
type Event struct {
	ID   uint64
	Type string
	Data any
}

// Bus fans events out to all current subscribers.
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

// droppableProgress marks high-frequency progress events: never retained for
// replay, and dropped (not a disconnect) when a subscriber can't keep up.
func droppableProgress(t string) bool {
	return t == BackupProgress || t == AppInstallProgress
}

// New creates a Bus. Its random epoch marks event IDs as belonging to this
// process; a Last-Event-ID from another epoch cannot be replayed and forces a
// client resync.
func New() *Bus {
	epoch := make([]byte, 4)
	_, _ = rand.Read(epoch)
	return &Bus{epoch: hex.EncodeToString(epoch), subs: map[int]chan Event{}}
}

// Epoch identifies this process's event-ID sequence on the wire.
func (b *Bus) Epoch() string { return b.epoch }

// Subscribe returns live events plus retained lifecycle events after afterID.
// complete is false when the caller must reload authoritative snapshots.
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

// Unsubscribe removes and closes a subscriber.
func (b *Bus) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// Emit never blocks producers. Stale progress may drop; a subscriber that
// cannot accept lifecycle is closed so it reconnects and replays or resyncs.
func (b *Bus) Emit(typ string, data any) {
	e := Event{Type: typ, Data: data}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	e.ID = b.nextID
	if !droppableProgress(e.Type) {
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
		if droppableProgress(e.Type) {
			continue // stale progress is worthless; the next frame supersedes it
		}
		delete(b.subs, id)
		close(ch) // reconnect + replay is safer than silently losing lifecycle
	}
}
