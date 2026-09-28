// Package ios speaks usbmuxd, lockdown and the lockdown services of iPhones and iPads.
package ios

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"howett.net/plist"
)

// maxFrame bounds a message; a larger claim is refused before allocating.
const maxFrame = 32 << 20

// PlistConn exchanges u32 big-endian length-prefixed property lists.
type PlistConn struct {
	net.Conn
	format int  // plist.XMLFormat or plist.BinaryFormat, for sent messages
	torn   bool // an I/O failure or interruption left the stream mid-message
}

// errTorn refuses a stream an earlier failure left mid-message.
var errTorn = fmt.Errorf("connection interrupted mid-message: %w", net.ErrClosed)

// NewPlistConn sends in format and receives either format.
func NewPlistConn(conn net.Conn, format int) *PlistConn {
	return &PlistConn{Conn: conn, format: format}
}

func (c *PlistConn) Send(v any) error {
	if c.torn {
		return errTorn
	}
	body, err := plist.Marshal(v, c.format)
	if err != nil {
		return fmt.Errorf("encode plist: %w", err)
	}
	if err := WriteFrame(c.Conn, body); err != nil {
		c.torn = true
		return err
	}
	return nil
}

// Recv returns a reply's Error as *DeviceError. Only a frame that fails to
// arrive tears the stream; one that fails to decode does not.
func (c *PlistConn) Recv(v any) error {
	if c.torn {
		return errTorn
	}
	body, err := ReadFrame(c.Conn)
	if err != nil {
		c.torn = true
		return err
	}
	if err := replyError(body); err != nil {
		return err
	}
	return decodePlist(body, v)
}

func (c *PlistConn) Exchange(ctx context.Context, request, reply any) error {
	return c.bound(ctx, func() error {
		if err := c.Send(request); err != nil {
			return err
		}
		return c.Recv(reply)
	})
}

func (c *PlistConn) SendContext(ctx context.Context, v any) error {
	return c.bound(ctx, func() error { return c.Send(v) })
}

func (c *PlistConn) RecvContext(ctx context.Context, v any) error {
	return c.bound(ctx, func() error { return c.Recv(v) })
}

func (c *PlistConn) bound(ctx context.Context, exchange func() error) error {
	release := Bind(ctx, c.Conn)
	err := exchange()
	if !release() {
		c.torn = true
	}
	return err
}

// WriteFrame writes the frame in one call, so TLS sends it as one record.
func WriteFrame(w io.Writer, body []byte) error {
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	_, err := w.Write(frame)
	return err
}

func ReadFrame(r io.Reader) ([]byte, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > maxFrame {
		return nil, fmt.Errorf("%w: frame of %d bytes", ErrProtocol, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, noEOF(err)
	}
	return body, nil
}

func decodePlist(body []byte, v any) error {
	if _, err := plist.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	return nil
}

// noEOF reports a stream that ended mid-message as truncated.
func noEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
