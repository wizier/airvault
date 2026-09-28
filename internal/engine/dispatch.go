package engine

import (
	"log/slog"
	"sync"
)

// latestDispatcher delivers state updates on its own goroutine so a slow
// consumer never stalls the producer; it only needs the newest state.
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

// A consumer panic must not stop the dispatcher.
func callHost(name string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("engine callback panicked", "callback", name, "panic", recovered)
		}
	}()
	fn()
}
