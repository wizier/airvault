package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/domain"
)

type resourceMode uint8

const (
	resourceRead resourceMode = iota
	resourceWrite
)

type resourceRequest struct {
	key  string
	mode resourceMode
}

// resourceState is one resource's current holders. holder/since are diagnostics
// for the rejection log only — exact for a writer, stale for readers.
type resourceState struct {
	writer  bool
	readers int
	holder  string
	since   time.Time
}

// operationManager atomically leases resources and is the admission half of
// operation lifecycle. Domain workers remain owned by their use cases.
type operationManager struct {
	mu        sync.Mutex
	resources map[string]*resourceState
}

func newOperationManager() *operationManager {
	return &operationManager{resources: make(map[string]*resourceState)}
}

// acquire takes every request or none and never waits, so a caller already
// holding a lease can take another without risking deadlock. holder names the
// operation in the rejection a later caller reads. release is idempotent.
func (c *operationManager) acquire(holder string, requests ...resourceRequest) (release func(), err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, req := range requests {
		state := c.resources[req.key]
		if state == nil {
			continue
		}
		if state.writer {
			return nil, busyWith(req.key, state)
		}
		if req.mode == resourceWrite && state.readers > 0 {
			return nil, busyWith(req.key, state)
		}
	}

	for _, req := range requests {
		state := c.resources[req.key]
		if state == nil {
			state = &resourceState{holder: holder, since: time.Now()}
			c.resources[req.key] = state
		}
		if req.mode == resourceWrite {
			state.writer = true
		} else {
			state.readers++
		}
	}
	return sync.OnceFunc(func() { c.release(requests) }), nil
}

// busyWith explains the rejection: a writer is named, readers are only counted
// (the one that created the state may have left). Unwraps to domain.ErrBusy —
// only that stable code crosses the HTTP edge.
func busyWith(key string, state *resourceState) error {
	if state.writer {
		return fmt.Errorf("%w: %s holds %s for %s",
			domain.ErrBusy, state.holder, key, time.Since(state.since).Round(time.Second))
	}
	return fmt.Errorf("%w: %d reader(s) hold %s", domain.ErrBusy, state.readers, key)
}

func (c *operationManager) release(requests []resourceRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, req := range requests {
		state := c.resources[req.key]
		if state == nil {
			continue
		}
		if req.mode == resourceWrite {
			state.writer = false
		} else {
			state.readers--
		}
		if !state.writer && state.readers == 0 {
			delete(c.resources, req.key)
		}
	}
}

// The device resource is an admission policy, not a native session: backups and
// AFC reads share it, restores and mutations take it exclusively. Independent
// reads (hardware, console) skip it; devicefs caps real AFC sessions per phone.
func deviceWriteResource(udid string) resourceRequest {
	return resourceRequest{key: "device:" + udid, mode: resourceWrite}
}

func deviceReadResource(udid string) resourceRequest {
	return resourceRequest{key: "device:" + udid, mode: resourceRead}
}

func snapshotReadResource(source string) resourceRequest {
	return resourceRequest{key: "snapshot:" + source, mode: resourceRead}
}

func snapshotWriteResource(source string) resourceRequest {
	return resourceRequest{key: "snapshot:" + source, mode: resourceWrite}
}
