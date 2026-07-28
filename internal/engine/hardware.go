package engine

import (
	"math"
	"strings"
)

// The Rust engine is a thin transport: av_device_hardware dumps the raw lockdown
// root, the com.apple.disk_usage domain and the AppleSmartBattery gas-gauge as
// they come off the phone. Selecting and interpreting fields happens here.

type rawDeviceInfo struct {
	Lockdown  rawLockdown  `json:"lockdown"`
	DiskUsage rawDiskUsage `json:"diskUsage"`
	Battery   rawBattery   `json:"battery"`
	// com.apple.fmip IsAssociated; nil when the read was unavailable.
	FindMy *bool `json:"findMy"`
}

type rawLockdown struct {
	SerialNumber     string       `json:"SerialNumber"`
	ProductType      string       `json:"ProductType"`
	ModelNumber      string       `json:"ModelNumber"`
	HardwareModel    string       `json:"HardwareModel"`
	RegionInfo       string       `json:"RegionInfo"`
	WiFiAddress      string       `json:"WiFiAddress"`
	BluetoothAddress string       `json:"BluetoothAddress"`
	PhoneNumber      string       `json:"PhoneNumber"`
	BuildVersion     string       `json:"BuildVersion"`
	TimeZone         string       `json:"TimeZone"`
	IMEI             string       `json:"InternationalMobileEquipmentIdentity"`
	IMEI2            string       `json:"InternationalMobileEquipmentIdentity2"`
	Carriers         []rawCarrier `json:"CarrierBundleInfoArray"`
}

type rawCarrier struct {
	Bundle string `json:"CFBundleIdentifier"`
	Slot   string `json:"Slot"`
}

type rawDiskUsage struct {
	TotalDataCapacity   uint64 `json:"TotalDataCapacity"`
	AmountDataAvailable uint64 `json:"AmountDataAvailable"`
	TotalDataAvailable  uint64 `json:"TotalDataAvailable"`
	PhotoUsage          uint64 `json:"PhotoUsage"`
	MediaCacheUsage     uint64 `json:"MediaCacheUsage"`
}

type rawBattery struct {
	MaxCapacity                      int64  `json:"MaxCapacity"`
	MaximumCapacityPercent           int64  `json:"MaximumCapacityPercent"`
	MaximumCapacityPercentWithSpaces int64  `json:"Maximum Capacity Percent"`
	CycleCount                       int64  `json:"CycleCount"`
	DesignCapacity                   int64  `json:"DesignCapacity"`
	AppleRawMaxCapacity              int64  `json:"AppleRawMaxCapacity"`
	NominalChargeCapacity            int64  `json:"NominalChargeCapacity"`
	Voltage                          int64  `json:"Voltage"`
	InstantAmperage                  int64  `json:"InstantAmperage"`
	Temperature                      int64  `json:"Temperature"`
	Serial                           string `json:"Serial"`
}

func (r rawDeviceInfo) toHardwareInfo() HardwareInfo {
	l, d, b := r.Lockdown, r.DiskUsage, r.Battery

	// AmountDataAvailable is the honest free figure (what Settings shows);
	// TotalDataAvailable counts purgeable space as free — fall back if absent.
	dataAvail := d.AmountDataAvailable
	if dataAvail == 0 {
		dataAvail = d.TotalDataAvailable
	}
	maxCap := b.fullChargeCapacity()

	return HardwareInfo{
		FindMyEnabled: r.FindMy,
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

		BatteryHealthPct:      b.healthPercent(),
		BatteryCycles:         nonNeg(b.CycleCount),
		BatteryDesignCapacity: nonNeg(b.DesignCapacity),
		BatteryMaxCapacity:    nonNeg(maxCap),
		BatteryVoltageMv:      nonNeg(b.Voltage),
		BatteryAmperageMa:     b.InstantAmperage,
		BatteryTemperature:    b.Temperature,
		BatterySerial:         b.Serial,
	}
}

// fullChargeCapacity returns the best available full-charge capacity in mAh.
// AppleSmartBattery overloads MaxCapacity: newer iOS reports a normalized
// 0..100 there, so values <= 100 are never treated as mAh.
func (b rawBattery) fullChargeCapacity() int64 {
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

// healthPercent prefers an explicit OS value when exposed. Diagnostics relay
// normally omits that private value, so fall back to the stable full-charge
// capacity divided by the design capacity. This is an estimate, not charge %.
func (b rawBattery) healthPercent() uint64 {
	reported := b.MaximumCapacityPercent
	if b.MaximumCapacityPercentWithSpaces > 0 {
		reported = b.MaximumCapacityPercentWithSpaces
	}
	if reported > 0 {
		return clampedPercent(reported)
	}

	full := b.fullChargeCapacity()
	if full <= 0 || b.DesignCapacity <= 0 {
		return 0
	}
	return clampedPercent(int64(math.Round(float64(full) * 100 / float64(b.DesignCapacity))))
}

func clampedPercent(percent int64) uint64 {
	switch {
	case percent <= 0:
		return 0
	case percent >= 100:
		return 100
	default:
		return uint64(percent)
	}
}

// parseSIMs builds one SIM per CarrierBundleInfoArray entry, joining the slot's
// IMEI (kOne -> imei1, kTwo -> imei2). Empty slots are dropped.
func parseSIMs(carriers []rawCarrier, imei1, imei2 string) []SIM {
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

// friendlyCarrier turns a carrier bundle id into "Name (CC)": "com.apple.MTS_ru"
// -> "MTS (RU)". The trailing "_xx" is the region; CarrierDefault/empty yields "".
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

// slotLabel maps the raw slot id to a display label; unknown slots drop the "k".
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

func nonNeg(n int64) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}
