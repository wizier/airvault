package ios

import (
	"context"
	"fmt"
	"net"

	"howett.net/plist"
)

// The mobileactivationd daemon takes one command per connection, so each of
// these closes conn.
const ActivationService = "com.apple.mobileactivationd"

// ActivationState is the phone's own state, as "Unactivated" or "Activated".
func ActivationState(ctx context.Context, conn net.Conn) (string, error) {
	var reply struct {
		Value string `plist:"Value"`
	}
	request := map[string]any{"Command": "GetActivationStateRequest"}
	if err := activationCommand(ctx, conn, request, &reply); err != nil {
		return "", err
	}
	if reply.Value == "" {
		return "", fmt.Errorf("GetActivationStateRequest: %w: no Value", ErrProtocol)
	}
	return reply.Value, nil
}

// ActivationSessionInfo is what Apple's drmHandshake takes, as an XML plist.
func ActivationSessionInfo(ctx context.Context, conn net.Conn) ([]byte, error) {
	return activationXML(ctx, conn, map[string]any{"Command": "CreateTunnel1SessionInfoRequest"})
}

// ActivationInfo is the activation request signed with Apple's handshake
// reply, as an XML plist.
func ActivationInfo(ctx context.Context, conn net.Conn, handshake []byte) ([]byte, error) {
	return activationXML(ctx, conn, map[string]any{"Command": "CreateTunnel1ActivationInfoRequest", "Value": handshake})
}

// Activate applies Apple's activation record; the daemon accepts it without a
// Value.
func Activate(ctx context.Context, conn net.Conn, record []byte, headers map[string]string) error {
	request := map[string]any{"Command": "HandleActivationInfoWithSessionRequest", "Value": record}
	if len(headers) > 0 {
		request["ActivationResponseHeaders"] = headers
	}
	return activationCommand(ctx, conn, request, &struct{}{})
}

func activationXML(ctx context.Context, conn net.Conn, request map[string]any) ([]byte, error) {
	var reply struct {
		Value any `plist:"Value"`
	}
	if err := activationCommand(ctx, conn, request, &reply); err != nil {
		return nil, err
	}
	if reply.Value == nil {
		return nil, fmt.Errorf("%s: %w: no Value", request["Command"], ErrProtocol)
	}
	return plist.MarshalIndent(reply.Value, plist.XMLFormat, "\t")
}

func activationCommand(ctx context.Context, conn net.Conn, request map[string]any, reply any) error {
	defer conn.Close()
	if err := NewPlistConn(conn, plist.XMLFormat).Exchange(ctx, request, reply); err != nil {
		return fmt.Errorf("%s: %w", request["Command"], err)
	}
	return nil
}
