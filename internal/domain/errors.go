// Package domain holds cross-cutting error sentinels and value types shared by
// the storage, service and handler layers.
package domain

import "errors"

var (
	// ErrNotFound is returned when a row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrDeviceOffline is returned when a backup is requested for a device that
	// is not currently reachable.
	ErrDeviceOffline = errors.New("device offline")
	// ErrPairingRequired means the device is known but its last definitive
	// lockdown verdict says this host is no longer trusted.
	ErrPairingRequired = errors.New("pairing required")
	// ErrPairingCleanup means the device-side action may have succeeded but the
	// private host record could not be removed from persistent storage.
	ErrPairingCleanup = errors.New("pairing cleanup failed")
	// ErrBusy is returned when an operation cannot acquire all device/snapshot resources.
	ErrBusy = errors.New("resource busy")
	// ErrCancelled is a terminal operation outcome, not a successful no-op.
	ErrCancelled = errors.New("operation cancelled")
	// ErrOperationState means a live operation cannot perform the requested
	// transition, for example cancellation after commit has begun.
	ErrOperationState = errors.New("operation state conflict")
)

// ValidationError is a 422 with a stable public code and a server-side
// diagnostic. Message never crosses the HTTP boundary.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// ActionError carries a stable public code plus the underlying implementation
// error for an operation that reached the phone but could not be completed.
// Only Code crosses the HTTP boundary; Cause remains available to diagnostics.
type ActionError struct {
	Code  string
	Cause error
}

func (e *ActionError) Error() string {
	if e.Cause == nil {
		return e.Code
	}
	return e.Code + ": " + e.Cause.Error()
}
func (e *ActionError) Unwrap() error { return e.Cause }

// NewActionError attaches a stable public code while retaining the
// implementation error for logs and errors.Is/errors.As.
func NewActionError(code string, cause error) *ActionError {
	return &ActionError{Code: code, Cause: cause}
}
