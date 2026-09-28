package engine

import (
	"log/slog"
	"sync"
)

type ProgressPhase uint8

const (
	ProgressPhaseTransfer ProgressPhase = iota
	ProgressPhaseSealing
)

// Progress is transport progress only. BytesDone is the operation's running
// total and never decreases.
type Progress struct {
	Phase     ProgressPhase
	Percent   int
	BytesDone int64
}

// transferProgress merges partial updates into the Progress the service
// sees: a negative percent keeps the last one, and bytes never go back.
type transferProgress struct {
	mu   sync.Mutex
	last Progress
	sink *latestDispatcher[Progress]
}

func newTransferProgress(fn func(Progress)) *transferProgress {
	return &transferProgress{sink: newLatestDispatcher("backup progress", fn)}
}

func (p *transferProgress) submit(phase ProgressPhase, percent float64, bytes uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last.Phase = phase
	if percent >= 0 {
		p.last.Percent = int(percent)
	}
	p.last.BytesDone = max(p.last.BytesDone, int64(bytes))
	p.sink.submit(p.last)
}

// close delivers the last update before returning.
func (p *transferProgress) close() { p.sink.close() }

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
