package events

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

const (
	DeviceAdded     = "device.added"
	DeviceUpdated   = "device.updated"
	DeviceRemoved   = "device.removed"
	DeviceOnline    = "device.online"
	DeviceOffline   = "device.offline"
	BackupStarted   = "backup.started"
	BackupProgress  = "backup.progress"
	BackupDone      = "backup.completed"
	BackupFailed    = "backup.failed"
	BackupCancelled = "backup.cancelled"
	BackupCatalog   = "backup.catalog"
	AppCatalog      = "app.catalog"
	PairChanged     = "pair.changed"
	PairTrust       = "pair.trust"
	MuxerChanged    = "muxer.changed"
	StreamReset     = "stream.reset"
)

type Event struct {
	ID   uint64
	Type string
	Data any
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

func droppableProgress(t string) bool {
	return t == BackupProgress
}

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

// Emit never blocks producers. Stale progress may drop, since the next frame
// supersedes it; a subscriber that cannot accept lifecycle is closed so it
// reconnects and replays or resyncs rather than silently losing it.
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
			continue
		}
		delete(b.subs, id)
		close(ch)
	}
}
