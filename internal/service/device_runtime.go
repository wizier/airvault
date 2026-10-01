package service

import (
	"cmp"
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

type deviceRuntime struct {
	presence   engine.Connection // "" while the muxer does not list the device
	screen     screenLockState
	lockedAt   time.Time
	unlockedAt time.Time // when the current unlock began; meaningful while screenUnlocked
	activation string
}

// Anything but a seen unlock counts as the lock screen.
func (r *deviceRuntime) lockScreen() bool { return r.screen != screenUnlocked }

type connectionTransition struct {
	udid string
	from engine.Connection
	to   engine.Connection
}

// MuxerStatus is what netmuxd last proved: whether it answers, the devices
// it sees and, when it does not, why.
type MuxerStatus struct {
	Up    bool   `json:"up"`
	USB   int    `json:"usb"`
	WiFi  int    `json:"wifi"`
	Error string `json:"error,omitempty"`
}

// The mutex is separate from Service.runMu so presence callbacks never share a
// lock with backup state. changed is closed and renewed on every connection
// change.
type deviceRuntimeStore struct {
	mu      sync.RWMutex
	devices map[string]*deviceRuntime
	changed chan struct{}
	muxer   MuxerStatus
	checked bool // the muxer has been checked at least once
}

func newDeviceRuntimeStore() *deviceRuntimeStore {
	return &deviceRuntimeStore{
		devices: map[string]*deviceRuntime{},
		changed: make(chan struct{}),
	}
}

func (s *deviceRuntimeStore) connection(udid string) engine.Connection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r := s.devices[udid]; r != nil {
		return r.presence
	}
	return ""
}

func (s *deviceRuntimeStore) connectionWait(udid string) (bool, <-chan struct{}) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.devices[udid]
	return r != nil && r.presence != "", s.changed
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

func (s *deviceRuntimeStore) applyPresence(presence map[string]engine.Connection) []connectionTransition {
	s.mu.Lock()
	defer s.mu.Unlock()
	var transitions []connectionTransition
	for udid, r := range s.devices {
		if _, still := presence[udid]; !still {
			delete(s.devices, udid)
			if r.presence != "" {
				transitions = append(transitions, connectionTransition{udid: udid, from: r.presence})
			}
		}
	}
	for udid, transport := range presence {
		r := s.devices[udid]
		if r == nil {
			r = &deviceRuntime{}
			s.devices[udid] = r
		}
		if r.presence != transport {
			transitions = append(transitions, connectionTransition{udid: udid, from: r.presence, to: transport})
			if r.presence == engine.ConnectionWiFi {
				// Lock signals only arrive over Wi-Fi: what was seen there goes stale.
				r.screen, r.lockedAt, r.unlockedAt = screenLockUnknown, time.Time{}, time.Time{}
			}
			r.presence = transport
		}
	}
	if len(transitions) == 0 {
		return nil
	}
	close(s.changed)
	s.changed = make(chan struct{})
	slices.SortFunc(transitions, func(a, b connectionTransition) int {
		return cmp.Or(cmp.Compare(a.udid, b.udid), cmp.Compare(a.to, b.to))
	})
	return transitions
}

func (s *deviceRuntimeStore) muxerStatus() MuxerStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.muxer
}

// applyMuxer records a muxer check; flipped means Up changed, as it does at
// the first check.
func (s *deviceRuntimeStore) applyMuxer(state engine.PresenceState) (status MuxerStatus, changed, flipped bool) {
	next := MuxerStatus{Up: state.MuxUp}
	for _, device := range state.Devices {
		switch device.Connection {
		case engine.ConnectionUSB:
			next.USB++
		case engine.ConnectionWiFi:
			next.WiFi++
		}
	}
	if state.Err != nil {
		next.Error = state.Err.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	flipped = !s.checked || s.muxer.Up != next.Up
	changed = flipped || s.muxer != next
	s.muxer, s.checked = next, true
	return next, changed, flipped
}

// The first unlock after a (re)connect counts as a change too.
func (s *deviceRuntimeStore) applyScreenLock(
	udid string,
	signal engine.ScreenLockSignal,
	now time.Time,
	pairWindow time.Duration,
) (changed, lockScreen, applied bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.devices[udid]
	if r == nil || r.presence != engine.ConnectionWiFi {
		return false, false, false
	}
	wasLockScreen := r.lockScreen()
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
	if wasLockScreen && !r.lockScreen() {
		r.unlockedAt = now
	}
	return wasLockScreen != r.lockScreen(), r.lockScreen(), true
}

// Empty means the read failed: keep the last known state, as for the identity
// fields.
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

// No online/offline transition: the caller publishes device.removed.
func (s *deviceRuntimeStore) removeLocal(udid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.devices[udid]; ok {
		delete(s.devices, udid)
		close(s.changed)
		s.changed = make(chan struct{})
	}
}
