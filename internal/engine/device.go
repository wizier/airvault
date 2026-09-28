package engine

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/wizier/airvault/internal/ios"
)

// The service calls without deadlines, so every device call is bounded here.
const (
	muxTimeout        = 5 * time.Second  // one muxer request
	probeTimeout      = 5 * time.Second  // cheap reads: battery, the battery gauge
	discoveryTimeout  = 4 * time.Second  // one device's discovery reads
	uiCallTimeout     = 15 * time.Second // reads and commands a user waits on
	connectTimeout    = 20 * time.Second // setting up a long-lived stream
	deviceWorkTimeout = 60 * time.Second // app lists, icons, activation
	pollInterval      = 2 * time.Second  // reconnecting a lost muxer
)

func call[T any](ctx context.Context, timeout time.Duration, operation string, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	value, err := fn(ctx)
	return value, failure(ctx, operation, err)
}

func do(ctx context.Context, timeout time.Duration, operation string, fn func(context.Context) error) error {
	_, err := call(ctx, timeout, operation, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}

func (e *Engine) devices(ctx context.Context) ([]ios.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, muxTimeout)
	defer cancel()
	devices, err := e.mux.Devices(ctx)
	return ios.PreferUSB(devices), err
}

func (e *Engine) device(ctx context.Context, udid string) (ios.Device, error) {
	if err := validateUDID(udid); err != nil {
		return ios.Device{}, err
	}
	devices, err := e.devices(ctx)
	if err != nil {
		return ios.Device{}, err
	}
	for _, device := range devices {
		if device.UDID == udid {
			return device, nil
		}
	}
	return ios.Device{}, fmt.Errorf("%s: %w", udid, errDeviceNotFound)
}

// record reads AirVault's own record: another host's pairing does not count.
func (e *Engine) record(udid string) (*ios.PairRecord, error) {
	record, err := e.pairs.Load(udid)
	if record == nil && err == nil {
		err = fmt.Errorf("no AirVault pairing record for %s: %w", udid, ios.ErrInvalidHostID)
	}
	return record, err
}

type session struct {
	*ios.Lockdown
	device ios.Device
	record *ios.PairRecord
}

func (e *Engine) openSession(ctx context.Context, udid string) (*session, error) {
	device, err := e.device(ctx, udid)
	if err != nil {
		return nil, err
	}
	record, err := e.record(udid)
	if err != nil {
		return nil, err
	}
	lockdown, err := ios.DialLockdown(ctx, e.mux, device)
	if err != nil {
		return nil, err
	}
	if err := lockdown.StartSession(ctx, record); err != nil {
		_ = lockdown.Close()
		return nil, err
	}
	return &session{Lockdown: lockdown, device: device, record: record}, nil
}

func (s *session) dial(ctx context.Context, mux ios.Mux, name string, escrowBag []byte) (net.Conn, error) {
	service, err := s.StartService(ctx, name, escrowBag)
	if err != nil {
		return nil, err
	}
	return ios.DialService(ctx, mux, s.device, service, s.record)
}

func (e *Engine) openService(ctx context.Context, udid, name string) (net.Conn, error) {
	session, err := e.openSession(ctx, udid)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	return session.dial(ctx, e.mux, name, nil)
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
