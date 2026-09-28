package engine

import (
	"context"
	"sync"

	"github.com/wizier/airvault/internal/ios"
)

// HardwareReport is raw device facts; interpreting them is the service's job.
type HardwareReport struct {
	Lockdown  LockdownValues
	DiskUsage DiskUsage
	Battery   BatteryGauge
	// com.apple.fmip IsAssociated; nil when the read was unavailable.
	FindMy *bool
}

type LockdownValues struct {
	SerialNumber     string          `plist:"SerialNumber"`
	ProductType      string          `plist:"ProductType"`
	ModelNumber      string          `plist:"ModelNumber"`
	HardwareModel    string          `plist:"HardwareModel"`
	RegionInfo       string          `plist:"RegionInfo"`
	WiFiAddress      string          `plist:"WiFiAddress"`
	BluetoothAddress string          `plist:"BluetoothAddress"`
	PhoneNumber      string          `plist:"PhoneNumber"`
	BuildVersion     string          `plist:"BuildVersion"`
	TimeZone         string          `plist:"TimeZone"`
	IMEI             string          `plist:"InternationalMobileEquipmentIdentity"`
	IMEI2            string          `plist:"InternationalMobileEquipmentIdentity2"`
	Carriers         []CarrierBundle `plist:"CarrierBundleInfoArray"`
}

type CarrierBundle struct {
	Bundle string `plist:"CFBundleIdentifier"`
	Slot   string `plist:"Slot"`
}

type DiskUsage struct {
	TotalDataCapacity   uint64 `plist:"TotalDataCapacity"`
	AmountDataAvailable uint64 `plist:"AmountDataAvailable"`
	TotalDataAvailable  uint64 `plist:"TotalDataAvailable"`
	PhotoUsage          uint64 `plist:"PhotoUsage"`
	MediaCacheUsage     uint64 `plist:"MediaCacheUsage"`
}

type BatteryGauge struct {
	MaxCapacity                      int64  `plist:"MaxCapacity"`
	MaximumCapacityPercent           int64  `plist:"MaximumCapacityPercent"`
	MaximumCapacityPercentWithSpaces int64  `plist:"Maximum Capacity Percent"`
	CycleCount                       int64  `plist:"CycleCount"`
	DesignCapacity                   int64  `plist:"DesignCapacity"`
	AppleRawMaxCapacity              int64  `plist:"AppleRawMaxCapacity"`
	NominalChargeCapacity            int64  `plist:"NominalChargeCapacity"`
	Voltage                          int64  `plist:"Voltage"`
	InstantAmperage                  int64  `plist:"InstantAmperage"`
	Temperature                      int64  `plist:"Temperature"`
	Serial                           string `plist:"Serial"`
}

// HardwareReport requires only the session: each read is bounded on its own
// and a failed one leaves its part zero.
func (e *Engine) HardwareReport(ctx context.Context, device DeviceID) (HardwareReport, error) {
	return call(ctx, uiCallTimeout, "hardware report", func(ctx context.Context) (HardwareReport, error) {
		session, err := e.openSession(ctx, string(device))
		if err != nil {
			return HardwareReport{}, err
		}
		defer session.Close()
		var report HardwareReport
		var wg sync.WaitGroup
		wg.Go(func() { report.Battery = e.batteryGauge(ctx, string(device)) })

		// The lockdown reads share one connection, and a read that times out
		// leaves it torn: the ones after it are skipped.
		reads := []func(context.Context) error{
			func(ctx context.Context) (err error) {
				report.Lockdown, err = ios.Value[LockdownValues](ctx, session.Lockdown, "", "")
				return err
			},
			func(ctx context.Context) (err error) {
				report.DiskUsage, err = ios.Value[DiskUsage](ctx, session.Lockdown, "com.apple.disk_usage", "")
				return err
			},
			func(ctx context.Context) error {
				associated, err := ios.Value[bool](ctx, session.Lockdown, "com.apple.fmip", "IsAssociated")
				if err == nil {
					report.FindMy = &associated
				}
				return err
			},
		}
		for _, read := range reads {
			readCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			_ = read(readCtx)
			timedOut := readCtx.Err() != nil
			cancel()
			if timedOut {
				break
			}
		}
		wg.Wait()
		return report, nil
	})
}

// batteryGauge is short and best effort: the relay can hang on Wi-Fi.
func (e *Engine) batteryGauge(ctx context.Context, udid string) BatteryGauge {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := e.openService(ctx, udid, ios.DiagnosticsService)
	if err != nil {
		return BatteryGauge{}
	}
	diagnostics := ios.NewDiagnostics(conn)
	defer diagnostics.Close()
	gauge, _ := ios.IORegistry[BatteryGauge](ctx, diagnostics, "AppleSmartBattery")
	return gauge
}
