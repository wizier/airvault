package engine

import (
	"context"
	"io"
	"sync"
)

// pull is the engine streams' lifecycle: the Open context and Close both
// interrupt next; a stream fails once, then reports io.ErrClosedPipe.
type pull[T any] struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	next   func(context.Context) (T, error)
	close  func()

	mu     sync.Mutex // serializes next with Close
	failed bool
}

func newPull[T any](ctx context.Context, next func(context.Context) (T, error), close func()) *pull[T] {
	ctx, cancel := context.WithCancelCause(ctx)
	return &pull[T]{ctx: ctx, cancel: cancel, next: next, close: close}
}

func (p *pull[T]) Next() (T, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var zero T
	if p.ctx.Err() != nil {
		return zero, context.Cause(p.ctx)
	}
	if p.failed {
		return zero, io.ErrClosedPipe
	}
	value, err := p.next(p.ctx)
	if err != nil {
		p.failed = true
		if p.ctx.Err() != nil {
			return zero, context.Cause(p.ctx)
		}
		return zero, err
	}
	return value, nil
}

func (p *pull[T]) Close() error {
	p.cancel(io.ErrClosedPipe)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.close != nil {
		p.close()
		p.close = nil
	}
	return nil
}
