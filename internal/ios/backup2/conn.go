// Package backup2 speaks mobilebackup2, the Finder backup service, over DeviceLink.
package backup2

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"time"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
)

const Service = "com.apple.mobilebackup2"

const emptyParameter = "___EmptyParameterString___"

// closeTimeout bounds the farewell: the device answers DLMessageDisconnect by
// closing its end, and one that does not is cut off.
const closeTimeout = 2 * time.Second

type Conn struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
}

// Open runs the DeviceLink and mobilebackup2 handshakes.
func Open(ctx context.Context, conn net.Conn) (*Conn, error) {
	c := &Conn{conn: conn, reader: bufio.NewReaderSize(conn, 256<<10), writer: bufio.NewWriterSize(conn, 256<<10)}
	release := ios.Bind(ctx, conn)
	err := c.handshake()
	if !release() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("mobilebackup2 handshake: %w", err)
	}
	return c, nil
}

func (c *Conn) handshake() error {
	if tag, _, err := c.recv(); err != nil || tag != "DLMessageVersionExchange" {
		return unexpected(tag, err)
	}
	if err := c.send([]any{"DLMessageVersionExchange", "DLVersionsOk", 400}); err != nil {
		return err
	}
	if tag, _, err := c.recv(); err != nil || tag != "DLMessageDeviceReady" {
		return unexpected(tag, err)
	}
	if err := c.sendMessage("Hello", map[string]any{"SupportedProtocolVersions": []any{2.0, 2.1}}); err != nil {
		return err
	}
	tag, message, err := c.recv()
	reply, _ := at(message, 1).(*Dict)
	if err != nil || tag != "DLMessageProcessMessage" || reply == nil || reply.Get("MessageName") != "Response" {
		return unexpected(tag, err)
	}
	if code, _ := reply.Get("ErrorCode").(int64); code != 0 {
		return fmt.Errorf("%w: Hello answered error %d", ios.ErrProtocol, code)
	}
	if _, ok := reply.Get("ProtocolVersion").(float64); !ok {
		return fmt.Errorf("%w: Hello answered no protocol version", ios.ErrProtocol)
	}
	return nil
}

// Close ends the conversation as Finder does: DLMessageDisconnect, then the
// device's own hang-up, awaited briefly. After a failed write the stream may
// stop mid-message, so no farewell follows it.
func (c *Conn) Close() error {
	_ = c.conn.SetDeadline(time.Now().Add(closeTimeout))
	if c.writer.Flush() == nil && c.send([]any{"DLMessageDisconnect", emptyParameter}) == nil {
		_, _ = io.Copy(io.Discard, c.reader)
	}
	return c.conn.Close()
}

// Backup asks the device to back itself up into the host's source directory.
func (c *Conn) Backup(ctx context.Context, target, source string) error {
	return c.request(ctx, "Backup", map[string]any{"TargetIdentifier": target, "SourceIdentifier": source})
}

type RestoreOptions struct {
	Reboot                 bool
	CopyBackup             bool // the device copies the backup aside first
	PreserveSettings       bool
	SystemFiles            bool
	RemoveItemsNotRestored bool
	Password               string
}

func (c *Conn) Restore(ctx context.Context, target, source string, options RestoreOptions) error {
	wire := map[string]any{
		"RestoreShouldReboot":     options.Reboot,
		"RestoreDontCopyBackup":   !options.CopyBackup,
		"RestorePreserveSettings": options.PreserveSettings,
		"RestoreSystemFiles":      options.SystemFiles,
		"RemoveItemsNotRestored":  options.RemoveItemsNotRestored,
	}
	if options.Password != "" {
		wire["Password"] = options.Password
	}
	return c.request(ctx, "Restore", map[string]any{"TargetIdentifier": target, "SourceIdentifier": source, "Options": wire})
}

// ChangePassword sets the backup password when old is empty, removes it
// when new is empty. The passwords go at the top level, as Finder sends them.
func (c *Conn) ChangePassword(ctx context.Context, target, old, new string) error {
	fields := map[string]any{"TargetIdentifier": target}
	if old != "" {
		fields["OldPassword"] = old
	}
	if new != "" {
		fields["NewPassword"] = new
	}
	return c.request(ctx, "ChangePassword", fields)
}

func (c *Conn) request(ctx context.Context, name string, fields map[string]any) error {
	release := ios.Bind(ctx, c.conn)
	defer release()
	return c.sendMessage(name, fields)
}

// Outcome waits for the device's final answer, skipping what comes before
// it; nil when the device disconnected without one.
func (c *Conn) Outcome(ctx context.Context) (*Dict, error) {
	release := ios.Bind(ctx, c.conn)
	defer release()
	for {
		tag, message, err := c.recv()
		switch {
		case err != nil:
			return nil, err
		case tag == "DLMessageProcessMessage":
			outcome, _ := at(message, 1).(*Dict)
			return outcome, nil
		case tag == "DLMessageDisconnect":
			return nil, nil
		}
	}
}

func (c *Conn) sendMessage(name string, fields map[string]any) error {
	message := map[string]any{"MessageName": name}
	maps.Copy(message, fields)
	return c.send([]any{"DLMessageProcessMessage", message})
}

func (c *Conn) send(message []any) error {
	body, err := plist.Marshal(message, plist.BinaryFormat)
	if err != nil {
		return fmt.Errorf("encode DeviceLink message: %w", err)
	}
	if err := ios.WriteFrame(c.writer, body); err != nil {
		return err
	}
	return c.writer.Flush()
}

func (c *Conn) recv() (string, []any, error) {
	body, err := ios.ReadFrame(c.reader)
	if err != nil {
		return "", nil, err
	}
	value, err := decode(body)
	if err != nil {
		return "", nil, err
	}
	message, _ := value.([]any)
	tag, ok := at(message, 0).(string)
	if !ok {
		return "", nil, fmt.Errorf("%w: DeviceLink message without a tag", ios.ErrProtocol)
	}
	return tag, message, nil
}

func decode(body []byte) (any, error) {
	if bytes.HasPrefix(body, []byte("bplist00")) {
		return decodeBinary(body)
	}
	var value any
	if _, err := plist.Unmarshal(body, &value); err != nil {
		return nil, fmt.Errorf("%w: %v", ios.ErrProtocol, err)
	}
	return ordered(value), nil
}

func ordered(value any) any {
	switch v := value.(type) {
	case map[string]any:
		dict := &Dict{Keys: slices.Sorted(maps.Keys(v))}
		for _, key := range dict.Keys {
			dict.Values = append(dict.Values, ordered(v[key]))
		}
		return dict
	case []any:
		for i := range v {
			v[i] = ordered(v[i])
		}
		return v
	case uint64:
		return int64(v)
	}
	return value
}

func at(message []any, i int) any {
	if i < len(message) {
		return message[i]
	}
	return nil
}

func unexpected(tag string, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: unexpected DeviceLink message %q", ios.ErrProtocol, tag)
}
