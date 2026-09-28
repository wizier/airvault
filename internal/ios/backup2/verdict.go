package backup2

import (
	"cmp"
	"fmt"

	"github.com/wizier/airvault/internal/ios"
)

// Error is the device refusing a request, with its MBErrorDomain code.
type Error struct {
	Code        int64
	Description string
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Description, e.Code) }

const (
	CodeWrongPassword = 207
	CodeDeviceLocked  = 208 // the passcode was not entered on the phone
	CodeFindMyEnabled = 211
)

// Verdict reads the device's final answer: nil for success, *Error for a
// refusal, ErrProtocol when there is no readable one.
func Verdict(outcome *Dict) error {
	if outcome == nil {
		return fmt.Errorf("%w: no final device verdict", ios.ErrProtocol)
	}
	code, ok := outcome.Get("ErrorCode").(int64)
	switch {
	case !ok:
		return fmt.Errorf("%w: device verdict without an ErrorCode", ios.ErrProtocol)
	case code == 0:
		return nil
	}
	description, _ := outcome.Get("ErrorDescription").(string)
	return &Error{Code: code, Description: cmp.Or(description, "the device refused the request")}
}
