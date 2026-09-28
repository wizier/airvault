package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/wizier/airvault/internal/ios"
)

func (e *Engine) ProbeMux(ctx context.Context) (bool, error) {
	probe, cancel := context.WithTimeout(ctx, muxTimeout)
	defer cancel()
	up := e.mux.Probe(probe) == nil
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return up, nil
}

func (e *Engine) ListPresence(ctx context.Context) ([]DevicePresence, error) {
	devices, err := e.devices(ctx)
	if err != nil {
		return nil, failure(ctx, "list devices", err)
	}
	return presenceOf(devices), nil
}

func presenceOf(devices []ios.Device) []DevicePresence {
	presence := make([]DevicePresence, len(devices))
	for i, device := range devices {
		presence[i] = DevicePresence{DeviceID: DeviceID(device.UDID), Connection: "usb"}
		if device.Connection == ios.ConnectionNetwork {
			presence[i].Connection = "wifi"
		}
	}
	return presence
}

// InspectDevices reads devices in parallel, each bounded: one unreachable
// phone must not stall the refresh.
func (e *Engine) InspectDevices(ctx context.Context) ([]DeviceInfo, error) {
	devices, err := e.devices(ctx)
	if err != nil {
		return nil, failure(ctx, "list devices", err)
	}
	infos := make([]DeviceInfo, len(devices))
	var wg sync.WaitGroup
	for i, device := range devices {
		wg.Go(func() { infos[i] = e.inspect(ctx, device) })
	}
	wg.Wait()
	return infos, nil
}

func (e *Engine) inspect(ctx context.Context, device ios.Device) DeviceInfo {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	info := DeviceInfo{DeviceID: DeviceID(device.UDID), Name: device.UDID, PairingState: PairingStateUnknown}
	lockdown, err := ios.DialLockdown(ctx, e.mux, device)
	if err != nil {
		return info
	}
	defer lockdown.Close()

	// The pairing verdict first: a later failing read must not erase it.
	info.PairingState = e.pairingState(ctx, lockdown, device.UDID)
	name, errName := nonEmpty(ctx, lockdown, "DeviceName")
	productType, errType := nonEmpty(ctx, lockdown, "ProductType")
	version, errVersion := nonEmpty(ctx, lockdown, "ProductVersion")
	if errors.Join(errName, errType, errVersion) == nil {
		info.Name, info.ProductType, info.IOSVersion = name, productType, version
		info.MetadataKnown = true
	}
	if info.PairingState == PairingStatePaired {
		if encrypted, err := ios.Value[bool](ctx, lockdown, "com.apple.mobile.backup", "WillEncrypt"); err == nil {
			info.Encrypted, info.FlagsKnown = encrypted, true
		}
	}
	info.ActivationState, _ = nonEmpty(ctx, lockdown, "ActivationState")
	return info
}

// pairingState is unknown unless the device answers: a transport failure
// proves nothing.
func (e *Engine) pairingState(ctx context.Context, lockdown *ios.Lockdown, udid string) PairingState {
	record, err := e.pairs.Load(udid)
	switch {
	case err != nil:
		return PairingStateUnknown
	case record == nil:
		return PairingStateUnpaired
	}
	err = lockdown.StartSession(ctx, record)
	switch {
	case err == nil:
		return PairingStatePaired
	case errors.Is(err, ios.ErrInvalidHostID):
		return PairingStateUnpaired
	}
	return PairingStateUnknown
}

func nonEmpty(ctx context.Context, lockdown *ios.Lockdown, key string) (string, error) {
	value, err := ios.Value[string](ctx, lockdown, "", key)
	if err == nil && value == "" {
		err = fmt.Errorf("%w: empty %s", ios.ErrProtocol, key)
	}
	return value, err
}

func (e *Engine) ListUSBDevices(ctx context.Context) ([]USBDevice, error) {
	devices, err := e.devices(ctx)
	if err != nil {
		return nil, failure(ctx, "list devices", err)
	}
	var attached []ios.Device
	for _, device := range devices {
		if device.Connection == ios.ConnectionUSB {
			attached = append(attached, device)
		}
	}
	usb := make([]USBDevice, len(attached))
	var wg sync.WaitGroup
	for i, device := range attached {
		usb[i] = USBDevice{DeviceID: DeviceID(device.UDID), Name: device.UDID}
		wg.Go(func() {
			if name, err := e.deviceName(ctx, device); err == nil {
				usb[i].Name = name
			}
		})
	}
	wg.Wait()
	return usb, nil
}

func (e *Engine) deviceName(ctx context.Context, device ios.Device) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	lockdown, err := ios.DialLockdown(ctx, e.mux, device)
	if err != nil {
		return "", err
	}
	defer lockdown.Close()
	return nonEmpty(ctx, lockdown, "DeviceName")
}

func (e *Engine) Battery(ctx context.Context, device DeviceID) (Battery, error) {
	return call(ctx, probeTimeout, "battery read", func(ctx context.Context) (Battery, error) {
		session, err := e.openSession(ctx, string(device))
		if err != nil {
			return Battery{}, err
		}
		defer session.Close()
		domain, err := ios.Value[map[string]any](ctx, session.Lockdown, "com.apple.mobile.battery", "")
		if err != nil {
			return Battery{}, err
		}
		level, ok := domain["BatteryCurrentCapacity"].(uint64)
		if !ok {
			return Battery{}, fmt.Errorf("%w: no BatteryCurrentCapacity", ios.ErrProtocol)
		}
		charging, known := domain["ExternalConnected"].(bool)
		if !known {
			charging, _ = domain["BatteryIsCharging"].(bool)
		}
		return Battery{Charging: charging, Level: int(level)}, nil
	})
}
