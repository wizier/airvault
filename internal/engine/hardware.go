package engine

// HardwareReport is av_device_hardware's raw dump: the lockdown root, the
// com.apple.disk_usage domain and the AppleSmartBattery gas gauge as they come
// off the phone. Selecting and interpreting fields is the service's job.
type HardwareReport struct {
	Lockdown  LockdownValues `json:"lockdown"`
	DiskUsage DiskUsage      `json:"diskUsage"`
	Battery   BatteryGauge   `json:"battery"`
	// com.apple.fmip IsAssociated; nil when the read was unavailable.
	FindMy *bool `json:"findMy"`
}

type LockdownValues struct {
	SerialNumber     string          `json:"SerialNumber"`
	ProductType      string          `json:"ProductType"`
	ModelNumber      string          `json:"ModelNumber"`
	HardwareModel    string          `json:"HardwareModel"`
	RegionInfo       string          `json:"RegionInfo"`
	WiFiAddress      string          `json:"WiFiAddress"`
	BluetoothAddress string          `json:"BluetoothAddress"`
	PhoneNumber      string          `json:"PhoneNumber"`
	BuildVersion     string          `json:"BuildVersion"`
	TimeZone         string          `json:"TimeZone"`
	IMEI             string          `json:"InternationalMobileEquipmentIdentity"`
	IMEI2            string          `json:"InternationalMobileEquipmentIdentity2"`
	Carriers         []CarrierBundle `json:"CarrierBundleInfoArray"`
}

type CarrierBundle struct {
	Bundle string `json:"CFBundleIdentifier"`
	Slot   string `json:"Slot"`
}

type DiskUsage struct {
	TotalDataCapacity   uint64 `json:"TotalDataCapacity"`
	AmountDataAvailable uint64 `json:"AmountDataAvailable"`
	TotalDataAvailable  uint64 `json:"TotalDataAvailable"`
	PhotoUsage          uint64 `json:"PhotoUsage"`
	MediaCacheUsage     uint64 `json:"MediaCacheUsage"`
}

type BatteryGauge struct {
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
