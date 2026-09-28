package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
	"github.com/wizier/airvault/internal/ios/backup2"
	"github.com/wizier/airvault/internal/objectstore"
)

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
