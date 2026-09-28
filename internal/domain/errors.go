package domain

import "errors"

var (
	ErrNotFound      = errors.New("not found")
	ErrDeviceOffline = errors.New("device offline")
	// The last definitive lockdown verdict says this host is no longer trusted.
	ErrPairingRequired = errors.New("pairing required")
	// The device-side action may have succeeded, but the host pair record could
	// not be removed from disk.
	ErrPairingCleanup = errors.New("pairing cleanup failed")
	ErrBusy           = errors.New("resource busy")
	// A terminal operation outcome, not a successful no-op.
	ErrCancelled = errors.New("operation cancelled")
	// A live operation cannot make the requested transition, e.g. cancel after
	// commit has begun.
	ErrOperationState = errors.New("operation state conflict")
)

// Message never crosses the HTTP boundary; only Code does.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// ActionError is for an operation that reached the phone but failed. Only Code
// crosses the HTTP boundary; Cause stays for logs.
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

func NewActionError(code string, cause error) *ActionError {
	return &ActionError{Code: code, Cause: cause}
}
