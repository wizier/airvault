package engine

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
