package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/wizier/airvault/internal/ios"
)

// A missing device item reads as fs.ErrNotExist; no other kind does.
func TestNotFoundIsErrNotExist(t *testing.T) {
	if !errors.Is(&Error{Kind: ErrorNotFound}, fs.ErrNotExist) {
		t.Fatal("ErrorNotFound must match fs.ErrNotExist")
	}
	if errors.Is(&Error{Kind: ErrorIntegrity}, fs.ErrNotExist) {
		t.Fatal("only ErrorNotFound matches fs.ErrNotExist")
	}
}

func TestClassify(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("operation: %w", err) }
	tests := []struct {
		err  error
		want ErrorKind
	}{
		{wrap(context.Canceled), ErrorCancelled},
		{wrap(os.ErrDeadlineExceeded), ErrorTimeout},
		{wrap(ios.ErrInvalidHostID), ErrorTrustRequired},
		{wrap(ios.ErrPairingDialogResponsePending), ErrorTrustRequired},
		{wrap(ios.ErrUserDeniedPairing), ErrorUserDenied},
		{wrap(ios.ErrPasswordProtected), ErrorDeviceLocked},
		{wrap(&ios.DeviceError{Code: "NSDebugDescription=Canceled by user."}), ErrorCancelled},
		{wrap(errDeviceNotFound), ErrorDeviceUnavailable},
		{wrap(ios.MuxConnectionRefused), ErrorDeviceUnavailable},
		{wrap(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), ErrorDeviceUnavailable},
		{wrap(io.ErrUnexpectedEOF), ErrorConnectionLost},
		{wrap(&net.OpError{Op: "read", Err: syscall.ECONNRESET}), ErrorConnectionLost},
		{wrap(ios.ErrProtocol), ErrorProtocol},
		{wrap(&ios.DeviceError{Code: "SomethingNew"}), ErrorProtocol},
		{wrap(ios.MuxBadVersion), ErrorProtocol},
		{errors.New("local disk trouble"), ErrorInternal},
	}
	for _, test := range tests {
		if got := classify(context.Background(), test.err); got != test.want {
			t.Errorf("classify(%v) = %d, want %d", test.err, got, test.want)
		}
	}
}

// An interrupted context explains the I/O error it caused.
func TestClassifyPrefersTheContext(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := classify(cancelled, os.ErrDeadlineExceeded); got != ErrorCancelled {
		t.Fatalf("cancelled context: %d, want ErrorCancelled", got)
	}
	expired, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	if got := classify(expired, io.ErrUnexpectedEOF); got != ErrorTimeout {
		t.Fatalf("expired context: %d, want ErrorTimeout", got)
	}
}

func TestFailureKeepsClassifiedErrors(t *testing.T) {
	original := &Error{Kind: ErrorBusy, Detail: "busy"}
	if got := failure(context.Background(), "op", fmt.Errorf("wrapped: %w", original)); !errors.Is(got, original) {
		t.Fatalf("failure reclassified %v", got)
	}
	if failure(context.Background(), "op", nil) != nil {
		t.Fatal("failure(nil) must be nil")
	}
}
