// Package engine talks to iOS devices over usbmuxd or netmuxd: discovery,
// pairing, device services, file access and backup transfers.
package engine

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/objectstore"
)

type Config struct {
	Objects     *objectstore.Store
	PairingRoot string
	MuxAddress  string
}

type Engine struct {
	mux     ios.Mux
	pairs   *pairStore
	afc     *afcPool
	objects *objectstore.Store
}

func New(config Config) (*Engine, error) {
	mux, err := ios.ParseMux(config.MuxAddress)
	if err != nil {
		return nil, err
	}
	pairingRoot, err := filepath.Abs(config.PairingRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve engine pairing root: %w", err)
	}
	return &Engine{mux: mux, pairs: &pairStore{root: pairingRoot}, afc: newAFCPool(), objects: config.Objects}, nil
}

func (e *Engine) Close() error {
	e.afc.close()
	return nil
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
	Phase   InstallPhase `json:"phase"`
	Percent int          `json:"percent"`
}

// BackupPasswordResult carries the encryption flag observed around
// ChangeBackupPassword. EncryptionKnown stays meaningful on failure: iOS may
// apply the mutation before DeviceLink reports a final verdict.
type BackupPasswordResult struct {
	EncryptionKnown bool
	Encrypted       bool
}

// PairingState separates a confirmed lockdown result from a probe that failed
// for an unrelated reason (transport, timeout, unreadable local record, etc.).
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
	DeviceID      DeviceID
	Name          string
	ProductType   string
	IOSVersion    string
	PairingState  PairingState
	MetadataKnown bool
	FlagsKnown    bool
	Encrypted     bool
	// ActivationState is the lockdown value ("Activated", "Unactivated",
	// "FactoryActivated", ...); empty when the read failed.
	ActivationState string
}

// DevicePresence is a cheap mux-local entry (no lockdown metadata).
// Connection is the transport the muxer prefers for it: "usb" or "wifi".
type DevicePresence struct {
	DeviceID   DeviceID
	Connection string
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
type SnapshotID string

type SnapshotRef struct {
	SourceID   DeviceID
	SnapshotID SnapshotID
}

type BuildSnapshotRequest struct {
	DeviceID   DeviceID
	SnapshotID SnapshotID
	// BaseSnapshotID selects the incremental base (same device); empty = full backup.
	BaseSnapshotID SnapshotID
}

type RestoreSnapshotRequest struct {
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
	PowerRestart PowerAction = iota
	PowerShutdown
	PowerSleep
)

// App is one installed application. FileSharing is true when the app exposes
// its Documents over house_arrest.
type App struct {
	BundleID    string `json:"bundleId"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	FileSharing bool   `json:"fileSharing,omitempty"`
}

// AFCEntry is raw metadata for one path in an AFC file tree.
type AFCEntry struct {
	IsDir    bool
	Size     int64
	Modified int64 // unix seconds, 0 if unknown
}

// AFCSource selects which device service opens an AFC connection.
type AFCSource int32

const (
	AFCMedia        AFCSource = iota // whole media partition (com.apple.afc)
	AFCAppDocuments                  // one app's Documents container (house_arrest)
)

// AFCSession is one sequential conversation on an AFC connection; the engine
// pools idle connections, so Close returns a healthy one for the next session.
// Open consumes the session because a file reader owns the transport until Close.
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

// AFCFile is a cancellable reader that owns its AFC connection until Close.
type AFCFile interface {
	io.ReadCloser
	Size() int64
	ModTime() time.Time // zero if unknown
	// SeekTo moves the device read cursor to an absolute offset (one round trip).
	SeekTo(offset int64) error
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

type ScreenLockSignal uint8

const (
	ScreenLockChanged ScreenLockSignal = iota + 1
	ScreenLockComplete
)

// PresenceState is the latest complete muxer state. Watchers may coalesce
// intermediate changes because every value is authoritative on its own.
type PresenceState struct {
	MuxUp   bool
	Devices []DevicePresence
}

// LockStream abstracts the owned notification stream so its supervisor can be
// tested without a device.
type LockStream interface {
	Next() (ScreenLockSignal, error)
	Close() error
}
