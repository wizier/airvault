package ios

import (
	"context"
	"fmt"
	"net"
	"time"

	"howett.net/plist"
)

const AssertionAgentService = "com.apple.mobile.assertion_agent"

// HoldWirelessSync keeps the device awake while conn stays open, for at most
// backstop: the assertion Finder's Wi-Fi sync takes.
func HoldWirelessSync(ctx context.Context, conn net.Conn, name string, backstop time.Duration) error {
	request := map[string]any{
		"CommandKey":          "CommandCreateAssertion",
		"AssertionTypeKey":    "AMDPowerAssertionTypeWirelessSync",
		"AssertionNameKey":    name,
		"AssertionTimeoutKey": backstop.Seconds(),
	}
	if err := NewPlistConn(conn, plist.BinaryFormat).Exchange(ctx, request, &struct{}{}); err != nil {
		return fmt.Errorf("create power assertion: %w", err)
	}
	return nil
}
