package engine

import (
	"context"
	"errors"
	"time"

	"github.com/wizier/airvault/internal/ios"
)

// DevicePresence is a device the muxer lists, by the transport it prefers:
// "usb" or "wifi".
type DevicePresence struct {
	DeviceID   DeviceID
	Connection string
}

// PresenceState is the muxer's complete state; Err says why it is down.
type PresenceState struct {
	MuxUp   bool
	Devices []DevicePresence
	Err     error
}

// muxHeartbeat is how long a quiet muxer is trusted before it must answer a
// fresh subscription, so a hung one reads as lost.
const muxHeartbeat = 30 * time.Second

// WatchPresence publishes the muxer state on connect, on each change, after
// each quiet heartbeat and once on its loss; it reconnects on its own, so only
// ctx and Close end it.
func (e *Engine) WatchPresence(ctx context.Context) Stream[PresenceState] {
	return e.watchPresence(ctx, muxHeartbeat, muxTimeout)
}

func (e *Engine) watchPresence(ctx context.Context, heartbeat, timeout time.Duration) Stream[PresenceState] {
	// Starts up, so a muxer that is down from the start is published too.
	watch := &presenceWatch{engine: e, heartbeat: heartbeat, timeout: timeout, up: true}
	return newPull(ctx, watch.next, watch.drop)
}

type presenceWatch struct {
	engine             *Engine
	heartbeat, timeout time.Duration
	listener           *ios.Listener
	up                 bool
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
		listen, cancel := context.WithTimeout(ctx, w.timeout)
		listener, err := w.engine.mux.Listen(listen)
		cancel()
		if err != nil {
			return w.lost(ctx, err)
		}
		w.listener = listener
		return w.snapshot(ctx)
	}
	quiet, cancel := context.WithTimeout(ctx, w.heartbeat)
	event, err := w.listener.Next(quiet)
	cancel()
	switch {
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		w.drop() // the next step subscribes again
		return PresenceState{}, false
	case err != nil:
		return w.lost(ctx, err)
	case event.Type == "Attached" || event.Type == "Detached" || event.Type == "Paired":
		return w.snapshot(ctx)
	}
	return PresenceState{}, false
}

func (w *presenceWatch) snapshot(ctx context.Context) (PresenceState, bool) {
	list, cancel := context.WithTimeout(ctx, w.timeout)
	devices, err := w.engine.devices(list)
	cancel()
	if err != nil {
		return w.lost(ctx, err)
	}
	w.up = true
	presence := make([]DevicePresence, len(devices))
	for i, device := range devices {
		presence[i] = DevicePresence{DeviceID: DeviceID(device.UDID), Connection: "usb"}
		if device.Connection == ios.ConnectionNetwork {
			presence[i].Connection = "wifi"
		}
	}
	return PresenceState{MuxUp: true, Devices: presence}, true
}

// lost publishes the first loss; later attempts wait a poll interval.
func (w *presenceWatch) lost(ctx context.Context, err error) (PresenceState, bool) {
	w.drop()
	if ctx.Err() != nil {
		return PresenceState{}, false
	}
	if w.up {
		w.up = false
		return PresenceState{MuxUp: false, Devices: []DevicePresence{}, Err: err}, true
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
