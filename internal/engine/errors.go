package engine

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
	"github.com/wizier/airvault/internal/ios/backup2"
	"github.com/wizier/airvault/internal/objectstore"
)

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

var errDeviceNotFound = errors.New("device not attached")

// failure wraps err as a classified *Error; a classified one passes through.
func failure(ctx context.Context, operation string, err error) error {
	if err == nil {
		return nil
	}
	var classified *Error
	if errors.As(err, &classified) {
		return err
	}
	return &Error{Kind: classify(ctx, err), Detail: operation + ": " + err.Error()}
}

// classify consults the context first: it explains the I/O errors it caused.
func classify(ctx context.Context, err error) ErrorKind {
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	var dial *net.OpError
	var status afc.Error
	switch {
	case errors.Is(err, context.Canceled):
		return ErrorCancelled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return ErrorTimeout
	case errors.Is(err, ios.ErrInvalidHostID), errors.Is(err, ios.ErrSessionInactive),
		errors.Is(err, ios.ErrPairingDialogResponsePending):
		return ErrorTrustRequired
	case errors.Is(err, ios.ErrUserDeniedPairing):
		return ErrorUserDenied
	case errors.Is(err, ios.ErrPasswordProtected), errors.Is(err, ios.ErrDeviceLocked),
		errors.Is(err, ios.ErrGetProhibited):
		return ErrorDeviceLocked
	case isCanceledByUser(err):
		return ErrorCancelled
	case errors.As(err, &status):
		return afcKind(status)
	case errors.Is(err, errDeviceNotFound), errors.Is(err, ios.MuxBadDevice),
		errors.Is(err, ios.MuxConnectionRefused), errors.As(err, &dial) && dial.Op == "dial":
		return ErrorDeviceUnavailable
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed),
		errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, syscall.ECONNABORTED):
		return ErrorConnectionLost
	case errors.Is(err, ios.ErrProtocol), errors.As(err, new(*ios.DeviceError)), errors.As(err, new(ios.MuxError)):
		return ErrorProtocol
	}
	return ErrorInternal
}

// isCanceledByUser recognizes the error of Cancel tapped on the phone.
func isCanceledByUser(err error) bool {
	var device *ios.DeviceError
	return errors.As(err, &device) && strings.Contains(device.Code, "Canceled by user")
}

func afcKind(status afc.Error) ErrorKind {
	switch status {
	case afc.ErrObjectNotFound:
		return ErrorNotFound
	case afc.ErrNoSpaceLeft:
		return ErrorDeviceStorageFull
	case afc.ErrOpTimeout:
		return ErrorTimeout
	case afc.ErrObjectBusy, afc.ErrOpWouldBlock, afc.ErrOpInProgress:
		return ErrorBusy
	case afc.ErrOpNotSupported:
		return ErrorUnsupported
	case afc.ErrServiceNotConnected, afc.ErrMuxError:
		return ErrorDeviceUnavailable
	}
	return ErrorProtocol
}

// storeFailure classifies an error of the host's backup store.
func storeFailure(operation string, err error) error {
	if err == nil {
		return nil
	}
	kind := ErrorInternal
	switch {
	case errors.Is(err, objectstore.ErrIntegrity), errors.Is(err, objectstore.ErrManifestCorrupt):
		kind = ErrorIntegrity
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		kind = ErrorStorageFull
	}
	return &Error{Kind: kind, Detail: operation + ": " + err.Error()}
}

// verdictFailure classifies the device refusing a mobilebackup2 request;
// an unconfirmed passcode fails a backup as not confirmed.
func verdictFailure(err error, backup bool) error {
	var refusal *backup2.Error
	if !errors.As(err, &refusal) {
		return err
	}
	kind := ErrorProtocol
	switch refusal.Code {
	case backup2.CodeWrongPassword:
		kind = ErrorInvalidBackupPassword
	case backup2.CodeDeviceLocked:
		kind = ErrorDeviceLocked
		if backup {
			kind = ErrorBackupNotConfirmed
		}
	case backup2.CodeFindMyEnabled:
		kind = ErrorFindMyEnabled
	}
	return &Error{Kind: kind, Detail: refusal.Error()}
}
