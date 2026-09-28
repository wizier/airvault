package engine

import (
	"context"

	"github.com/wizier/airvault/internal/ios"
)

// PresenceWatcher publishes the muxer state on connect and on each change,
// and one down state when the muxer goes away; it reconnects on its own.
type PresenceWatcher struct {
	*pull[PresenceState]
}

func (e *Engine) OpenPresenceWatcher(ctx context.Context) (*PresenceWatcher, error) {
	// Starts up, so a muxer that is down from the start is published too.
	watch := &presenceWatch{engine: e, up: true}
	return &PresenceWatcher{newPull(ctx, watch.next, watch.drop)}, nil
}

type presenceWatch struct {
	engine   *Engine
	listener *ios.Listener
	up       bool
}

func (w *presenceWatch) next(ctx context.Context) (PresenceState, error) {
	for ctx.Err() == nil {
		if state, publish := w.step(ctx); publish {
			return state, nil
		}
	}
	return PresenceState{}, ctx.Err()
}

func (w *presenceWatch) step(ctx context.Context) (PresenceState, bool) {
	if w.listener == nil {
		listen, cancel := context.WithTimeout(ctx, muxTimeout)
		listener, err := w.engine.mux.Listen(listen)
		cancel()
		if err != nil {
			return w.lost(ctx)
		}
		w.listener = listener
		return w.snapshot(ctx)
	}
	event, err := w.listener.Next(ctx)
	switch {
	case err != nil:
		return w.lost(ctx)
	case event.Type == "Attached" || event.Type == "Detached" || event.Type == "Paired":
		return w.snapshot(ctx)
	}
	return PresenceState{}, false
}

func (w *presenceWatch) snapshot(ctx context.Context) (PresenceState, bool) {
	devices, err := w.engine.devices(ctx)
	if err != nil {
		return w.lost(ctx)
	}
	w.up = true
	return PresenceState{MuxUp: true, Devices: presenceOf(devices)}, true
}

// lost publishes the first loss; later attempts wait a poll interval.
func (w *presenceWatch) lost(ctx context.Context) (PresenceState, bool) {
	w.drop()
	if ctx.Err() != nil {
		return PresenceState{}, false
	}
	if w.up {
		w.up = false
		return PresenceState{MuxUp: false, Devices: []DevicePresence{}}, true
	}
	sleep(ctx, pollInterval)
	return PresenceState{}, false
}

func (w *presenceWatch) drop() {
	if w.listener != nil {
		_ = w.listener.Close()
		w.listener = nil
	}
}
