package engine

import (
	"net/netip"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
)

const (
	afcIdleTimeout     = 30 * time.Second // a browsing burst's think time
	afcLifetime        = 5 * time.Minute  // bounds staleness: containers change, trust is revoked
	afcIdlePerKey      = 2                // a thumbnail batch overlapping a preview
	afcValidateAfter   = 2 * time.Second  // returned more recently than this: reused as is
	afcValidateTimeout = 2 * time.Second  // a healthy phone answers one stat quickly
)

type afcKey struct {
	udid     string
	source   AFCSource
	bundleID string
}

// afcTransport: a connection is reused only on the device's current transport.
type afcTransport struct {
	connection ios.Connection
	addr       netip.Addr
}

type afcOrigin struct {
	key       afcKey
	transport afcTransport
	created   time.Time
}

type idleAFC struct {
	id       uint64
	client   *afc.Client
	origin   afcOrigin
	returned time.Time
	timer    *time.Timer
}

// afcPool keeps idle connections so a browsing burst pays the handshake once.
type afcPool struct {
	now         func() time.Time
	idleTimeout time.Duration

	mu     sync.Mutex
	nextID uint64
	idle   map[afcKey][]*idleAFC // in return order: the last is the newest
	closed bool
}

func newAFCPool() *afcPool {
	return &afcPool{now: time.Now, idleTimeout: afcIdleTimeout, idle: map[afcKey][]*idleAFC{}}
}

func (p *afcPool) take(key afcKey, transport afcTransport) (entry *idleAFC, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := p.idle[key]
	now := p.now()
	kept := entries[:0]
	for _, entry := range entries {
		if entry.origin.transport != transport || now.Sub(entry.returned) >= p.idleTimeout || now.Sub(entry.origin.created) >= afcLifetime {
			p.discard(entry)
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 {
		delete(p.idle, key)
		return nil, false
	}
	entry = kept[len(kept)-1]
	entry.timer.Stop()
	p.idle[key] = kept[:len(kept)-1]
	return entry, true
}

// put closes anything unfit to reuse.
func (p *afcPool) put(origin afcOrigin, client *afc.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !client.Idle() || p.now().Sub(origin.created) >= afcLifetime {
		_ = client.Close()
		return
	}
	p.nextID++
	entry := &idleAFC{id: p.nextID, client: client, origin: origin, returned: p.now()}
	entry.timer = time.AfterFunc(p.idleTimeout, func() { p.expire(origin.key, entry.id) })
	entries := append(p.idle[origin.key], entry)
	if len(entries) > afcIdlePerKey {
		p.discard(entries[0])
		entries = entries[1:]
	}
	p.idle[origin.key] = entries
}

func (p *afcPool) expire(key afcKey, id uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := p.idle[key]
	for i, entry := range entries {
		if entry.id == id {
			p.discard(entry)
			p.idle[key] = append(entries[:i:i], entries[i+1:]...)
			return
		}
	}
}

func (p *afcPool) forget(udid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, entries := range p.idle {
		if key.udid == udid {
			for _, entry := range entries {
				p.discard(entry)
			}
			delete(p.idle, key)
		}
	}
}

func (p *afcPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for key, entries := range p.idle {
		for _, entry := range entries {
			p.discard(entry)
		}
		delete(p.idle, key)
	}
}

func (p *afcPool) discard(entry *idleAFC) {
	entry.timer.Stop()
	_ = entry.client.Close()
}
