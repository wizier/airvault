package service

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/engine"
)

type screenLockState uint8

const (
	screenLockUnknown screenLockState = iota
	screenUnlocked
	screenLocked
)

// deviceRuntime is the low-level, volatile evidence known about one muxer
// device. It deliberately contains no registry row, backup state or UI event
// policy; those belong to the Service layer.
type deviceRuntime struct {
	presence   string
	screen     screenLockState
	lockedAt   time.Time
	activation string
}

type connectionTransition struct {
	udid string
	from string
	to   string
}

// deviceRuntimeStore owns only live device evidence and its published
// connection projection. Keeping this mutex separate from Service.runMu stops
// presence callbacks from sharing a lock with backup business state.
type deviceRuntimeStore struct {
	mu          sync.RWMutex
	devices     map[string]*deviceRuntime
	connections map[string]string
	changed     chan struct{}
}

func newDeviceRuntimeStore() *deviceRuntimeStore {
	return &deviceRuntimeStore{
		devices:     map[string]*deviceRuntime{},
		connections: map[string]string{},
		changed:     make(chan struct{}),
	}
}

func (s *deviceRuntimeStore) connection(udid string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connections[udid]
}

func (s *deviceRuntimeStore) connectionWait(udid string) (bool, <-chan struct{}) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connections[udid] != "", s.changed
}

func (s *deviceRuntimeStore) snapshot() map[string]deviceRuntime {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]deviceRuntime, len(s.devices))
	for udid, r := range s.devices {
		out[udid] = *r
	}
	return out
}

// applyPresence replaces raw muxer evidence and returns the resulting
// connection projection. Pairing/registration policy is intentionally absent.
func (s *deviceRuntimeStore) applyPresence(presence map[string]string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for udid := range s.devices {
		if _, still := presence[udid]; !still {
			delete(s.devices, udid)
		}
	}
	for udid, transport := range presence {
		r := s.devices[udid]
		if r == nil {
			r = &deviceRuntime{}
			s.devices[udid] = r
		}
		r.presence = transport
	}
	return s.connectionsLocked()
}

func (s *deviceRuntimeStore) applyScreenLock(
	udid string,
	signal engine.ScreenLockSignal,
	now time.Time,
	pairWindow time.Duration,
) (changed, locked, applied bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.devices[udid]
	if r == nil || r.presence != "wifi" {
		return false, false, false
	}
	wasLocked := r.screen == screenLocked
	switch signal {
	case engine.ScreenLockComplete:
		r.screen = screenLocked
		r.lockedAt = now
	case engine.ScreenLockChanged:
		if now.Sub(r.lockedAt) <= pairWindow {
			r.screen = screenLocked
		} else {
			r.screen = screenUnlocked
		}
	default:
		return false, false, false
	}
	locked = r.screen == screenLocked
	return wasLocked != locked, locked, true
}

// applyActivation records the lockdown activation state; true when a listed
// device's value actually changed. Empty means the read failed — keep the
// last known state, same rule as the identity fields.
func (s *deviceRuntimeStore) applyActivation(udid, state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.devices[udid]
	if r == nil || state == "" || r.activation == state {
		return false
	}
	r.activation = state
	return true
}

func (s *deviceRuntimeStore) connectionsLocked() map[string]string {
	out := make(map[string]string, len(s.devices))
	for udid, r := range s.devices {
		out[udid] = r.presence
	}
	return out
}

// publish commits a validated projection and returns pure transitions. The
// Service layer decides which logs, database writes and domain events follow.
func (s *deviceRuntimeStore) publish(next map[string]string) []connectionTransition {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.connections
	if maps.Equal(old, next) {
		return nil
	}
	s.connections = maps.Clone(next)
	close(s.changed)
	s.changed = make(chan struct{})

	transitions := make([]connectionTransition, 0)
	for udid, connection := range next {
		if old[udid] != connection {
			transitions = append(transitions, connectionTransition{udid: udid, from: old[udid], to: connection})
		}
	}
	for udid, connection := range old {
		if next[udid] == "" {
			transitions = append(transitions, connectionTransition{udid: udid, from: connection})
			if r := s.devices[udid]; r != nil {
				r.screen, r.lockedAt = screenLockUnknown, time.Time{}
			}
		}
	}
	slices.SortFunc(transitions, func(a, b connectionTransition) int {
		if byDevice := cmp.Compare(a.udid, b.udid); byDevice != 0 {
			return byDevice
		}
		return cmp.Compare(a.to, b.to)
	})
	return transitions
}

// removeLocal is used after an explicit registry removal. It intentionally
// emits no online/offline transition; the caller publishes device.removed.
func (s *deviceRuntimeStore) removeLocal(udid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devices, udid)
	if _, ok := s.connections[udid]; ok {
		delete(s.connections, udid)
		close(s.changed)
		s.changed = make(chan struct{})
	}
}
