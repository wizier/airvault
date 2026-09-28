package ios

import (
	"context"
	"fmt"
	"net"

	"howett.net/plist"
)

const DiagnosticsService = "com.apple.mobile.diagnostics_relay"

type Diagnostics struct {
	conn *PlistConn
}

func NewDiagnostics(conn net.Conn) *Diagnostics {
	return &Diagnostics{conn: NewPlistConn(conn, plist.XMLFormat)}
}

func (d *Diagnostics) Close() error { return d.conn.Close() }

// Restart, Shutdown and Sleep return once the device accepted the command.
func (d *Diagnostics) Restart(ctx context.Context) error  { return d.power(ctx, "Restart") }
func (d *Diagnostics) Shutdown(ctx context.Context) error { return d.power(ctx, "Shutdown") }
func (d *Diagnostics) Sleep(ctx context.Context) error    { return d.power(ctx, "Sleep") }

func (d *Diagnostics) power(ctx context.Context, request string) error {
	_, err := diagnosticsRequest[struct{}](ctx, d, map[string]any{"Request": request})
	return err
}

func IORegistry[T any](ctx context.Context, d *Diagnostics, name string) (T, error) {
	return diagnosticsRequest[T](ctx, d, map[string]any{"Request": "IORegistry", "EntryName": name})
}

func diagnosticsRequest[T any](ctx context.Context, d *Diagnostics, request map[string]any) (T, error) {
	var reply struct {
		Status      string `plist:"Status"`
		Diagnostics struct {
			IORegistry T `plist:"IORegistry"`
		} `plist:"Diagnostics"`
	}
	name := request["Request"]
	if err := d.conn.Exchange(ctx, request, &reply); err != nil {
		return reply.Diagnostics.IORegistry, fmt.Errorf("diagnostics %s: %w", name, err)
	}
	if reply.Status != "Success" {
		return reply.Diagnostics.IORegistry, fmt.Errorf("diagnostics %s: %w: status %q", name, ErrProtocol, reply.Status)
	}
	return reply.Diagnostics.IORegistry, nil
}
