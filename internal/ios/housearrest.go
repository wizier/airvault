package ios

import (
	"context"
	"fmt"
	"net"

	"howett.net/plist"
)

const HouseArrestService = "com.apple.mobile.house_arrest"

// VendDocuments turns conn into AFC over one app's Documents container.
func VendDocuments(ctx context.Context, conn net.Conn, bundleID string) error {
	request := map[string]any{"Command": "VendDocuments", "Identifier": bundleID}
	if err := NewPlistConn(conn, plist.XMLFormat).Exchange(ctx, request, &struct{}{}); err != nil {
		return fmt.Errorf("vend documents of %s: %w", bundleID, err)
	}
	return nil
}
