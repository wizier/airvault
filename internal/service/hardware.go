package service

import (
	"cmp"
	"context"
	"log/slog"
	"math"
	"strings"

	"github.com/wizier/airvault/internal/engine"
)

// Every field is best-effort: keys an iOS version doesn't expose stay empty and
// are omitted from the JSON.
type HardwareInfo struct {
	Serial        string `json:"serial,omitempty"`
	ProductType   string `json:"productType,omitempty"`
	ModelNumber   string `json:"modelNumber,omitempty"`
	HardwareModel string `json:"hardwareModel,omitempty"`
	Region        string `json:"region,omitempty"`
	WifiMac       string `json:"wifiMac,omitempty"`
	BluetoothMac  string `json:"bluetoothMac,omitempty"`

	PhoneNumber  string `json:"phoneNumber,omitempty"`
	SIMs         []SIM  `json:"sims,omitempty"`
	BuildVersion string `json:"buildVersion,omitempty"`
	TimeZone     string `json:"timeZone,omitempty"`

	// com.apple.fmip IsAssociated, a restore blocker. Nil means the value could
	// not be read, never "off".
	FindMyEnabled *bool `json:"findMyEnabled,omitempty"`

	DiskDataCapacity  uint64 `json:"diskDataCapacity,omitempty"`  // data partition, bytes
	DiskDataAvailable uint64 `json:"diskDataAvailable,omitempty"` // free on data partition
	DiskPhotos        uint64 `json:"diskPhotos,omitempty"`        // photo library, bytes
	DiskMedia         uint64 `json:"diskMedia,omitempty"`         // media cache, bytes

	BatteryHealthPct      uint64 `json:"batteryHealthPct,omitempty"` // reported health %, or capacity-based estimate
	BatteryCycles         uint64 `json:"batteryCycles,omitempty"`
	BatteryDesignCapacity uint64 `json:"batteryDesignCapacity,omitempty"` // mAh
	BatteryMaxCapacity    uint64 `json:"batteryMaxCapacity,omitempty"`    // mAh, current full charge
	BatteryVoltageMv      uint64 `json:"batteryVoltageMv,omitempty"`
	BatteryAmperageMa     int64  `json:"batteryAmperageMa,omitempty"`  // negative = discharging
	BatteryTemperature    int64  `json:"batteryTemperature,omitempty"` // centi-°C as reported
	BatterySerial         string `json:"batterySerial,omitempty"`
}

type SIM struct {
	Slot    string `json:"slot,omitempty"` // "Primary"/"Secondary", set only on dual-SIM
	Carrier string `json:"carrier,omitempty"`
	IMEI    string `json:"imei,omitempty"`
}

func (s *Service) HardwareInfo(ctx context.Context, udid string) (HardwareInfo, error) {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return HardwareInfo{}, err
	}
	report, err := s.engine.HardwareReport(ctx, engine.DeviceID(udid))
	if err != nil {
		slog.WarnContext(ctx, "hardware info: engine", "udid", udid, "error", err)
		return HardwareInfo{}, newEngineActionError("hardware_query_failed", err)
	}
	return hardwareInfo(report), nil
}

func hardwareInfo(report engine.HardwareReport) HardwareInfo {
	l, d, b := report.Lockdown, report.DiskUsage, report.Battery

	// AmountDataAvailable is the honest free figure (what Settings shows);
	// TotalDataAvailable counts purgeable space as free — fall back if absent.
	dataAvail := cmp.Or(d.AmountDataAvailable, d.TotalDataAvailable)

	return HardwareInfo{
		FindMyEnabled: report.FindMy,
		Serial:        l.SerialNumber,
		ProductType:   l.ProductType,
		ModelNumber:   l.ModelNumber,
		HardwareModel: l.HardwareModel,
		Region:        l.RegionInfo,
		WifiMac:       l.WiFiAddress,
		BluetoothMac:  l.BluetoothAddress,
		PhoneNumber:   l.PhoneNumber,
		SIMs:          parseSIMs(l.Carriers, l.IMEI, l.IMEI2),
		BuildVersion:  l.BuildVersion,
		TimeZone:      l.TimeZone,

		DiskDataCapacity:  d.TotalDataCapacity,
		DiskDataAvailable: dataAvail,
		DiskPhotos:        d.PhotoUsage,
		DiskMedia:         d.MediaCacheUsage,

		BatteryHealthPct:      batteryHealthPercent(b),
		BatteryCycles:         uint64(max(b.CycleCount, 0)),
		BatteryDesignCapacity: uint64(max(b.DesignCapacity, 0)),
		BatteryMaxCapacity:    uint64(fullChargeCapacity(b)),
		BatteryVoltageMv:      uint64(max(b.Voltage, 0)),
		BatteryAmperageMa:     b.InstantAmperage,
		BatteryTemperature:    b.Temperature,
		BatterySerial:         b.Serial,
	}
}

// AppleSmartBattery overloads MaxCapacity: newer iOS reports a normalized
// 0..100 there, so values <= 100 are never treated as mAh.
func fullChargeCapacity(b engine.BatteryGauge) int64 {
	switch {
	case b.NominalChargeCapacity > 0:
		return b.NominalChargeCapacity
	case b.AppleRawMaxCapacity > 0:
		return b.AppleRawMaxCapacity
	case b.MaxCapacity > 100:
		return b.MaxCapacity
	default:
		return 0
	}
}

// Diagnostics relay normally omits the OS health value, so the fallback is
// full-charge over design capacity: an estimate, not charge %.
func batteryHealthPercent(b engine.BatteryGauge) uint64 {
	reported := b.MaximumCapacityPercent
	if b.MaximumCapacityPercentWithSpaces > 0 {
		reported = b.MaximumCapacityPercentWithSpaces
	}
	if reported > 0 {
		return uint64(min(reported, 100))
	}

	full := fullChargeCapacity(b)
	if full <= 0 || b.DesignCapacity <= 0 {
		return 0
	}
	estimate := int64(math.Round(float64(full) * 100 / float64(b.DesignCapacity)))
	return uint64(min(max(estimate, 0), 100))
}

func parseSIMs(carriers []engine.CarrierBundle, imei1, imei2 string) []SIM {
	var sims []SIM
	for i, c := range carriers {
		carrier := friendlyCarrier(c.Bundle)
		var imei string
		switch {
		case c.Slot == "kTwo":
			imei = imei2
		case c.Slot == "kOne":
			imei = imei1
		case i == 0:
			imei = imei1
		default:
			imei = imei2
		}
		if carrier == "" && imei == "" {
			continue
		}
		sims = append(sims, SIM{Slot: slotLabel(c.Slot), Carrier: carrier, IMEI: imei})
	}
	return sims
}

// "com.apple.MTS_ru" -> "MTS (RU)": the trailing "_xx" is the region.
func friendlyCarrier(bundleID string) string {
	name := strings.TrimPrefix(bundleID, "com.apple.")
	if name == "" || strings.EqualFold(name, "CarrierDefault") {
		return ""
	}
	if i := strings.LastIndexByte(name, '_'); i >= 0 {
		head, cc := name[:i], name[i+1:]
		if len(cc) >= 2 && len(cc) <= 3 && isASCIIAlpha(cc) {
			return head + " (" + strings.ToUpper(cc) + ")"
		}
	}
	return name
}

func slotLabel(slot string) string {
	switch slot {
	case "kOne":
		return "Primary"
	case "kTwo":
		return "Secondary"
	default:
		return strings.TrimLeft(slot, "k")
	}
}

func isASCIIAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
