// Package engine is the boundary to the iOS-device implementation: the Rust
// `idevice` shim linked via cgo. Bounded operations time out in Rust; owned
// streams bind their Open context to Close so cancellation interrupts native
// transport without freeing an in-flight handle.
package engine

import (
	"context"
	"errors"
	"io"
)

// Config is immutable process-boundary configuration. Transfer requests never
// repeat storage/provider settings.
type Config struct {
	BackupRoot  string
	PairingRoot string
	MuxAddress  string
}

type ErrorKind uint8

// Error is the stable device-engine failure contract. Detail is diagnostic;
// callers branch on Kind and never parse Error().
type Error struct {
	Kind   ErrorKind
	Detail string
}

func (e *Error) Error() string {
	if e == nil {
		return "device engine error"
	}
	if e.Detail != "" {
		return e.Detail
	}
	return "device engine operation failed"
}

// Is lets errors.Is(err, ErrDeviceUnreachable) match the stable unreachable
// classification without callers parsing diagnostic text.
func (e *Error) Is(target error) bool {
	return target == ErrDeviceUnreachable && e.Kind == ErrorDeviceUnavailable
}

type ProgressPhase uint8

const (
	ProgressPhaseTransfer ProgressPhase = iota
	ProgressPhaseSealing
)

// Progress is transport progress only. BytesDone is the operation's running
// total and never decreases.
type Progress struct {
	Phase     ProgressPhase
	Percent   int
	BytesDone int64
}

// InstallPhase identifies one device-side phase of installing an IPA.
type InstallPhase string

const (
	InstallPhaseStaging    InstallPhase = "staging"
	InstallPhaseInstalling InstallPhase = "installing"
)

// InstallProgress is phase-local progress reported by the device engine.
type InstallProgress struct {
	Phase   InstallPhase
	Percent int
}

// BackupPasswordResult carries the encryption flag observed around
// ChangeBackupPassword. EncryptionKnown stays meaningful on failure: iOS may
// apply the mutation before DeviceLink reports a final verdict.
type BackupPasswordResult struct {
	EncryptionKnown bool
	Encrypted       bool
}

// PairingState separates a confirmed lockdown result from a probe that failed
// for an unrelated reason (transport, timeout, corrupt local record, etc.).
type PairingState string

const (
	PairingStatePaired   PairingState = "paired"
	PairingStateUnpaired PairingState = "unpaired"
	PairingStateUnknown  PairingState = "unknown"
)

// DeviceInfo is a device the engine currently sees. MetadataKnown and
// FlagsKnown are false when the lockdown identity / encryption reads failed.
// Presence and transport come from PresenceState instead.
type DeviceInfo struct {
	DeviceID      DeviceID     `json:"udid"`
	Name          string       `json:"name"`
	ProductType   string       `json:"productType"`
	IOSVersion    string       `json:"iosVersion"`
	PairingState  PairingState `json:"pairingState"`
	MetadataKnown bool         `json:"metadataKnown"`
	FlagsKnown    bool         `json:"flagsKnown"`
	Encrypted     bool         `json:"encrypted"`
	// ActivationState is the lockdown value ("Activated", "Unactivated",
	// "FactoryActivated", ...); empty when the read failed.
	ActivationState string `json:"activationState"`
}

type Transport uint8

const (
	TransportUSB Transport = iota + 1
	TransportWiFi
)

func (t Transport) String() string {
	if t == TransportUSB {
		return "usb"
	}
	if t == TransportWiFi {
		return "wifi"
	}
	return ""
}

// DevicePresence is a cheap mux-local entry (no lockdown metadata).
type DevicePresence struct {
	DeviceID           DeviceID
	PreferredTransport Transport
}

// rawPresence is the shim's wire shape for one presence entry.
type rawPresence struct {
	UDID       string `json:"udid"`
	Connection string `json:"connection"`
}

func presenceFromRaw(raw []rawPresence) []DevicePresence {
	items := make([]DevicePresence, 0, len(raw))
	for _, item := range raw {
		transport := TransportWiFi
		if item.Connection == "usb" {
			transport = TransportUSB
		}
		items = append(items, DevicePresence{
			DeviceID: DeviceID(item.UDID), PreferredTransport: transport,
		})
	}
	return items
}

// USBDevice is a device reachable over USB (for the pairing wizard).
type USBDevice struct {
	DeviceID DeviceID `json:"udid"`
	Name     string   `json:"name"`
}

// Battery is the device power state (for the charging / min-battery gates).
type Battery struct {
	Charging bool `json:"charging"`
	Level    int  `json:"level"` // 0-100
}

type DeviceID string
type OperationID string
type SnapshotID string

type SnapshotRef struct {
	SourceID   DeviceID
	SnapshotID SnapshotID
}

type BuildSnapshotRequest struct {
	OperationID OperationID
	DeviceID    DeviceID
	SnapshotID  SnapshotID
	// BaseSnapshotID selects the incremental base (same device); empty = full backup.
	BaseSnapshotID SnapshotID
}

type RestoreSnapshotRequest struct {
	OperationID OperationID
	TargetID    DeviceID
	Snapshot    SnapshotRef
	Password    string
	SystemFiles bool
	Reboot      bool
	// Wire inverse of RestorePreserveSettings; true mirrors a Finder restore.
	SettingsFromBackup     bool
	RemoveItemsNotRestored bool
}

type PowerAction uint8

const (
	PowerRestart PowerAction = iota + 1
	PowerShutdown
	PowerSleep
)

// HardwareInfo is a hardware/storage/battery snapshot. Every field is
// best-effort — keys an iOS version doesn't expose stay zero/empty and are
// omitted from the JSON.
type HardwareInfo struct {
	Serial        string `json:"serial,omitempty"`
	ProductType   string `json:"productType,omitempty"` // lockdown identifier, e.g. "iPhone16,2"
	ModelNumber   string `json:"modelNumber,omitempty"`
	HardwareModel string `json:"hardwareModel,omitempty"`
	Region        string `json:"region,omitempty"` // model region suffix, e.g. "LL/A"
	WifiMac       string `json:"wifiMac,omitempty"`
	BluetoothMac  string `json:"bluetoothMac,omitempty"`

	PhoneNumber  string `json:"phoneNumber,omitempty"`
	SIMs         []SIM  `json:"sims,omitempty"` // one entry per active SIM slot
	BuildVersion string `json:"buildVersion,omitempty"`
	TimeZone     string `json:"timeZone,omitempty"`

	// FindMyEnabled reports com.apple.fmip IsAssociated — a restore blocker.
	// Nil means the value could not be read, never "off".
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

// SIM is one cellular slot: friendly carrier ("MTS (RU)") and that slot's IMEI.
type SIM struct {
	Slot    string `json:"slot,omitempty"` // "Primary"/"Secondary", set only on dual-SIM
	Carrier string `json:"carrier,omitempty"`
	IMEI    string `json:"imei,omitempty"`
}

// App is one installed application. FileSharing is true when the app exposes
// its Documents over house_arrest (drives the "Files" action). iOS does not
// report per-app disk size over installation_proxy.
type App struct {
	BundleID    string `json:"bundleId"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	FileSharing bool   `json:"fileSharing,omitempty"`
}

// AFCEntry is raw metadata for one path in an AFC file tree.
type AFCEntry struct {
	IsDir    bool  `json:"isDir"`
	Size     int64 `json:"size"`
	Modified int64 `json:"modified"` // unix seconds, 0 if unknown
}

// AFCSource selects which native service opens an AFC connection.
type AFCSource int32

const (
	AFCMedia        AFCSource = 0 // whole media partition (com.apple.afc)
	AFCAppDocuments AFCSource = 1 // one app's Documents container (house_arrest)
)

// AFCSession is one sequential, short-lived device connection. Open consumes
// the session because a file reader owns its dedicated transport until Close.
type AFCSession interface {
	List(path string) ([]string, error)
	Stat(path string) (AFCEntry, error)
	Open(path string) (AFCFile, error)
	// ReadSmall reads one whole small file without consuming the session, so one
	// session serves a batch of reads (thumbnails). Large files use Open.
	ReadSmall(path string) ([]byte, error)
	Remove(path string) error
	Close() error
}

// AFCFile is a cancellable reader backed by one dedicated AFC connection.
type AFCFile interface {
	io.ReadCloser
	Size() int64
}

// afcError adapts the typed engine error to the session-owner contract: the
// owning context's error wins, and a cancelled/closed native slot surfaces as
// context.Canceled (a concurrent Close raced the call). Other kinds pass through.
func afcError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var engineErr *Error
	if errors.As(err, &engineErr) && engineErr.Kind == ErrorCancelled {
		return context.Canceled
	}
	return err
}

// ConsoleLine is one structured record from the device's os_trace stream.
type ConsoleLine struct {
	Timestamp string `json:"ts"`
	Level     string `json:"level"` // notice | info | debug | error | fault
	Pid       uint32 `json:"pid"`
	Image     string `json:"image"`
	Message   string `json:"message"`
	Subsystem string `json:"subsystem,omitempty"`
	Category  string `json:"category,omitempty"`
}

type PairingOutcome string

// Pairing outcomes reported by AdvancePairing.
const (
	TrustPaired                  PairingOutcome = "paired"
	TrustPending                 PairingOutcome = "trust_pending"             // Trust dialog is up; poll again
	TrustLocked                  PairingOutcome = "locked"                    // phone must be unlocked first
	TrustDenied                  PairingOutcome = "denied"                    // user tapped "Don't Trust"
	TrustWiFiAuthorizationFailed PairingOutcome = "wifi_authorization_failed" // USB trust saved, mandatory Wi-Fi setup failed
	TrustError                   PairingOutcome = "error"
)

// ErrDeviceUnreachable is matched (via Error.Is) by any engine error of kind
// ErrorDeviceUnavailable: no session could be established, as opposed to a
// logical read failure. Presence itself still follows the muxer's device list.
var ErrDeviceUnreachable = errors.New("device unreachable")

type ScreenLockSignal uint8

const (
	ScreenLockChanged ScreenLockSignal = iota + 1
	ScreenLockComplete
)

type WallpaperScreen uint8

const (
	WallpaperHome WallpaperScreen = iota
	WallpaperLock
)

type MuxState uint8

const (
	MuxUnavailable MuxState = iota + 1
	MuxAvailable
)

// PresenceState is the latest complete muxer state. Watchers may coalesce
// intermediate changes because every value is authoritative on its own.
type PresenceState struct {
	MuxState MuxState
	Devices  []DevicePresence
}

// LockStream abstracts the owned notification stream so its supervisor can be
// tested without a device.
type LockStream interface {
	Next() (ScreenLockSignal, error)
	Close() error
}
