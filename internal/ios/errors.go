package ios

import (
	"errors"
	"fmt"
	"strconv"

	"howett.net/plist"
)

// ErrProtocol marks a device or muxer reply that breaks the protocol.
var ErrProtocol = errors.New("device protocol violation")

type DeviceError struct {
	Code        string // "InvalidHostID", "PasswordProtected", ...
	Description string // ErrorDescription, when the device gave one
}

func (e *DeviceError) Error() string {
	if e.Description != "" {
		return "device error " + e.Code + ": " + e.Description
	}
	return "device error " + e.Code
}

// Is matches by code, so the sentinels below work with errors.Is.
func (e *DeviceError) Is(target error) bool {
	t, ok := target.(*DeviceError)
	return ok && t.Code == e.Code
}

// Device error codes AirVault branches on.
var (
	ErrInvalidHostID                = &DeviceError{Code: "InvalidHostID"}
	ErrSessionInactive              = &DeviceError{Code: "SessionInactive"}
	ErrPasswordProtected            = &DeviceError{Code: "PasswordProtected"}
	ErrDeviceLocked                 = &DeviceError{Code: "DeviceLocked"}
	ErrGetProhibited                = &DeviceError{Code: "GetProhibited"}
	ErrPairingDialogResponsePending = &DeviceError{Code: "PairingDialogResponsePending"}
	ErrUserDeniedPairing            = &DeviceError{Code: "UserDeniedPairing"}
)

// replyError reads a reply's Error: a code string, or an integer that
// ErrorString describes.
func replyError(body []byte) error {
	var reply struct {
		Error            any    `plist:"Error"`
		ErrorString      string `plist:"ErrorString"`
		ErrorDescription string `plist:"ErrorDescription"`
	}
	if _, err := plist.Unmarshal(body, &reply); err != nil || reply.Error == nil {
		return nil
	}
	code := reply.ErrorString
	switch value := reply.Error.(type) {
	case string:
		code = value
	case uint64:
		if code == "" {
			code = strconv.FormatUint(value, 10)
		}
	case int64:
		if code == "" {
			code = strconv.FormatInt(value, 10)
		}
	default:
		return fmt.Errorf("%w: Error of type %T", ErrProtocol, value)
	}
	return &DeviceError{Code: code, Description: reply.ErrorDescription}
}

type MuxError uint64

const (
	MuxBadCommand        MuxError = 1
	MuxBadDevice         MuxError = 2 // no such device (any more)
	MuxConnectionRefused MuxError = 3 // nothing listens on that device port
	MuxBadVersion        MuxError = 6
)

func (e MuxError) Error() string {
	switch e {
	case MuxBadCommand:
		return "usbmuxd: bad command"
	case MuxBadDevice:
		return "usbmuxd: bad device"
	case MuxConnectionRefused:
		return "usbmuxd: connection refused by the device"
	case MuxBadVersion:
		return "usbmuxd: bad protocol version"
	default:
		return "usbmuxd: result " + strconv.FormatUint(uint64(e), 10)
	}
}
