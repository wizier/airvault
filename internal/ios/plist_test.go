package ios

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"howett.net/plist"
)

func TestFrameRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := buffer.Bytes(); !bytes.Equal(got, []byte{0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o'}) {
		t.Fatalf("frame = %v", got)
	}
	body, err := ReadFrame(&buffer)
	if err != nil || string(body) != "hello" {
		t.Fatalf("ReadFrame = %q, %v", body, err)
	}
}

// A size claim is checked before anything is allocated for it.
func TestReadFrameRefusesBadSizes(t *testing.T) {
	for _, size := range []uint32{0, maxFrame + 1, 1<<32 - 1} {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], size)
		if _, err := ReadFrame(bytes.NewReader(header[:])); !errors.Is(err, ErrProtocol) {
			t.Errorf("size %d: %v, want ErrProtocol", size, err)
		}
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 9, 'x'})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated body: %v, want io.ErrUnexpectedEOF", err)
	}
}

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 'x'})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		body, err := ReadFrame(bytes.NewReader(data))
		if err == nil && (len(body) == 0 || len(body) > len(data)) {
			t.Fatalf("read %d bytes from %d", len(body), len(data))
		}
	})
}

// A reply's Error becomes a *DeviceError that matches the sentinels.
func TestRecvReturnsDeviceErrors(t *testing.T) {
	tests := []struct {
		reply map[string]any
		want  error
		code  string
	}{
		{map[string]any{"Error": "InvalidHostID"}, ErrInvalidHostID, "InvalidHostID"},
		{map[string]any{"Error": uint64(3), "ErrorString": "PasswordProtected"}, ErrPasswordProtected, "PasswordProtected"},
		{map[string]any{"Error": uint64(7)}, nil, "7"},
	}
	for _, test := range tests {
		body, err := plist.Marshal(test.reply, plist.BinaryFormat)
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		_ = WriteFrame(&buffer, body)
		conn := NewPlistConn(&bufferConn{Buffer: &buffer}, plist.XMLFormat)
		err = conn.Recv(&struct{}{})
		var device *DeviceError
		if !errors.As(err, &device) || device.Code != test.code {
			t.Errorf("reply %v: %v, want device error %q", test.reply, err, test.code)
		}
		if test.want != nil && !errors.Is(err, test.want) {
			t.Errorf("reply %v: %v does not match %v", test.reply, err, test.want)
		}
	}
}

// Cancelling the context interrupts a blocked read; the stream is then not
// intact. A context that ends quietly leaves the connection reusable.
func TestBindInterruptsAndReleases(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	release := Bind(ctx, client)
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) && !isTimeout(err) {
		t.Fatalf("read under a cancelled context: %v", err)
	}
	if release() {
		t.Fatal("an interrupted connection reported intact")
	}

	quiet, peer := net.Pipe()
	defer quiet.Close()
	defer peer.Close()
	ctx, cancel = context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if release := Bind(ctx, quiet); !release() {
		t.Fatal("an untouched connection reported interrupted")
	}
	cancel()
	go func() { _, _ = peer.Write([]byte{1}) }()
	if _, err := quiet.Read(make([]byte, 1)); err != nil {
		t.Fatalf("a released connection must outlive its context: %v", err)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// bufferConn is a net.Conn over a buffer, for framing tests.
type bufferConn struct {
	*bytes.Buffer
	net.Conn
}

func (c *bufferConn) Read(p []byte) (int, error)  { return c.Buffer.Read(p) }
func (c *bufferConn) Write(p []byte) (int, error) { return c.Buffer.Write(p) }

// An exchange a context interrupted leaves the stream mid-message; the
// connection refuses further use instead of reading garbage.
func TestExchangeRefusesATornConnection(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() { _, _ = io.Copy(io.Discard, server) }()
	conn := NewPlistConn(client, plist.XMLFormat)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := conn.Exchange(ctx, map[string]any{"Request": "x"}, &struct{}{}); err == nil {
		t.Fatal("an exchange without a reply succeeded")
	}
	if err := conn.Exchange(context.Background(), map[string]any{"Request": "y"}, &struct{}{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("exchange on a torn connection = %v, want net.ErrClosed", err)
	}
}

// A frame that fails to arrive tears the stream; a complete reply that fails
// to decode does not.
func TestRecvTearsOnlyOnStreamFailures(t *testing.T) {
	var buffer bytes.Buffer
	_ = WriteFrame(&buffer, []byte("<plist><dict><key>n</key><string>text</string></dict></plist>"))
	conn := NewPlistConn(&bufferConn{Buffer: &buffer}, plist.XMLFormat)
	var typed struct {
		N int `plist:"n"`
	}
	if err := conn.Recv(&typed); !errors.Is(err, ErrProtocol) || conn.torn {
		t.Fatalf("undecodable reply: %v, torn %v", err, conn.torn)
	}
	buffer.Write([]byte{0, 0, 0, 9, 'x'})
	if err := conn.Recv(&typed); err == nil || !conn.torn {
		t.Fatalf("truncated frame: %v, torn %v", err, conn.torn)
	}
	if err := conn.Send(map[string]any{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("send on a torn stream = %v", err)
	}
}
