package engine

import (
	"context"
	"log/slog"
	"sync"
)

// nativeHandle is the single lifecycle for every opaque Rust stream/resource:
// cancel first, wait for the active FFI call, destroy last. Calls are serialized
// because each underlying protocol stream is sequential by definition.
type nativeHandle[T comparable] struct {
	mu      sync.Mutex
	calls   sync.Mutex
	value   T
	cancel  func(T)
	destroy func(T)
	done    chan struct{}
	stop    func() bool
}

// newNativeHandle binds the resource to the context passed to Open, so
// cancellation can interrupt a blocked native call immediately.
func newNativeHandle[T comparable](ctx context.Context, value T, cancel, destroy func(T)) *nativeHandle[T] {
	h := &nativeHandle[T]{value: value, cancel: cancel, destroy: destroy}
	h.mu.Lock()
	h.stop = context.AfterFunc(ctx, h.close)
	h.mu.Unlock()
	return h
}

func (h *nativeHandle[T]) enter() (value T, leave func(), ok bool) {
	h.calls.Lock()
	h.mu.Lock()
	value = h.value
	var zero T
	ok = value != zero
	h.mu.Unlock()
	if !ok {
		h.calls.Unlock()
		return zero, nil, false
	}
	return value, h.calls.Unlock, true
}

// detach transfers ownership to the active call. It must be invoked between
// enter and leave; a concurrent close wins by replacing value first.
func (h *nativeHandle[T]) detach(expected T) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.value != expected {
		return false
	}
	var zero T
	h.value = zero
	return true
}

func (h *nativeHandle[T]) stopContextClose() {
	h.mu.Lock()
	stop := h.stop
	h.stop = nil
	h.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (h *nativeHandle[T]) close() {
	h.mu.Lock()
	var zero T
	if h.value == zero {
		done := h.done
		h.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	value := h.value
	h.value = zero
	stop := h.stop
	h.stop = nil
	done := make(chan struct{})
	h.done = done
	h.mu.Unlock()

	if stop != nil {
		stop()
	}
	if h.cancel != nil {
		h.cancel(value)
	}
	h.calls.Lock()
	h.destroy(value)
	h.calls.Unlock()
	close(done)
}

// latestDispatcher keeps foreign threads out of application callbacks. Values
// are state updates, so a slow consumer needs only the newest one; Close still
// delivers that final value before returning.
type latestDispatcher[T any] struct {
	name string
	fn   func(T)

	mu      sync.Mutex
	updates chan T
	done    chan struct{}
}

func newLatestDispatcher[T any](name string, fn func(T)) *latestDispatcher[T] {
	if fn == nil {
		fn = func(T) {}
	}
	d := &latestDispatcher[T]{
		name:    name,
		fn:      fn,
		updates: make(chan T, 1),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(d.done)
		for value := range d.updates {
			callHost(d.name, func() { d.fn(value) })
		}
	}()
	return d
}

func (d *latestDispatcher[T]) submit(value T) {
	d.mu.Lock()
	select {
	case d.updates <- value:
	default:
		// The worker may consume between these selects; either way the
		// following send sees an empty one-slot channel.
		select {
		case <-d.updates:
		default:
		}
		d.updates <- value
	}
	d.mu.Unlock()
}

func (d *latestDispatcher[T]) close() {
	d.mu.Lock()
	close(d.updates)
	d.mu.Unlock()
	<-d.done
}

// A host callback must never unwind through C/Rust. Keeping recovery at this
// single dispatch point also lets later updates continue after a bad consumer.
func callHost(name string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("engine callback panicked", "callback", name, "panic", recovered)
		}
	}()
	fn()
}
