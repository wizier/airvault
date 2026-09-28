package engine

import "io/fs"

// ErrorKind says what happened; the service picks the public code by context.
type ErrorKind uint8

const (
	ErrorInvalidArgument   ErrorKind = iota + 1
	ErrorDeviceUnavailable           // not reachable: not attached, muxer down, no service
	ErrorDeviceLocked
	ErrorTrustRequired // no valid pairing with this host
	ErrorUserDenied
	ErrorBusy
	ErrorTimeout
	ErrorCancelled
	ErrorProtocol    // the device answered outside the protocol
	ErrorStorageFull // the host backup volume is full
	ErrorIntegrity   // stored backup data failed verification
	ErrorUnsupported
	ErrorInternal
	ErrorInvalidBackupPassword
	ErrorOutcomeUnknown // the request reached the device, its effect is unconfirmed
	ErrorFindMyEnabled
	ErrorBackupNotConfirmed
	ErrorConnectionLost    // an established device connection dropped mid-operation
	ErrorDeviceStorageFull // the device itself is out of space
	ErrorNotFound          // the device has no such item; matches fs.ErrNotExist
)

// Error's Detail is diagnostic: callers branch on Kind, never on the text.
type Error struct {
	Kind   ErrorKind
	Detail string
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return "device engine operation failed"
}

func (e *Error) Is(target error) bool {
	return target == fs.ErrNotExist && e.Kind == ErrorNotFound
}
