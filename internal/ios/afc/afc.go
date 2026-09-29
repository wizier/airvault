// Package afc speaks Apple File Conduit, the device file protocol.
package afc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strconv"
	"time"

	"github.com/wizier/airvault/internal/ios"
)

const Service = "com.apple.afc"

const (
	magic       = 0x4141504c36414643 // "CFA6LPAA"
	headerSize  = 40
	maxPacket   = 32 << 20
	MaxTransfer = 1 << 20
)

const (
	opStatus      = 0x01
	opReadDir     = 0x03
	opRemovePath  = 0x08
	opMakeDir     = 0x09
	opGetFileInfo = 0x0A
	opFileOpen    = 0x0D
	opFileOpenRes = 0x0E
	opFileRead    = 0x0F
	opFileWrite   = 0x10
	opFileSeek    = 0x11
	opFileClose   = 0x14
	opFileLock    = 0x1B
	opRemoveAll   = 0x22
)

type Mode uint64

const (
	ReadOnly  Mode = 1 // r
	ReadWrite Mode = 2 // r+, creating
	WriteOnly Mode = 3 // w, creating and truncating
)

// Lock operations, flock(2) with LOCK_NB.
type Lock uint64

const (
	LockExclusive Lock = 6  // LOCK_EX | LOCK_NB
	LockRelease   Lock = 12 // LOCK_UN | LOCK_NB
)

// Error is an AFC status; unlike I/O errors it leaves the connection usable.
type Error uint64

const (
	ErrObjectNotFound      Error = 8
	ErrObjectIsDir         Error = 9
	ErrServiceNotConnected Error = 11
	ErrOpTimeout           Error = 12
	ErrEndOfData           Error = 14
	ErrOpNotSupported      Error = 15
	ErrObjectBusy          Error = 17
	ErrNoSpaceLeft         Error = 18
	ErrOpWouldBlock        Error = 19
	ErrOpInProgress        Error = 22
	ErrMuxError            Error = 30
)

func (e Error) Error() string { return "afc status " + strconv.FormatUint(uint64(e), 10) }

func (e Error) Is(target error) bool {
	return e == ErrObjectNotFound && target == fs.ErrNotExist
}

var errTorn = fmt.Errorf("afc connection interrupted mid-packet: %w", net.ErrClosed)

// Client is one AFC connection. A failure below the status level (I/O, a
// malformed packet, an interrupted context) tears it for good.
type Client struct {
	conn   net.Conn
	packet uint64
	torn   bool
	open   int // descriptors opened and not yet closed
}

func New(conn net.Conn) *Client { return &Client{conn: conn} }

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) Torn() bool { return c.torn }

// Idle reports a connection fit to reuse: not torn, no descriptor open.
func (c *Client) Idle() bool { return !c.torn && c.open == 0 }

type FileInfo struct {
	Size    int64
	ModTime time.Time // zero when not reported
	IsDir   bool      // a symlink is not a directory
}

// List includes "." and "..".
func (c *Client) List(ctx context.Context, path string) ([]string, error) {
	reply, err := c.request(ctx, opReadDir, []byte(path), nil)
	if err != nil {
		return nil, err
	}
	return nulSeparated(reply.payload), nil
}

func (c *Client) Stat(ctx context.Context, path string) (FileInfo, error) {
	reply, err := c.request(ctx, opGetFileInfo, []byte(path), nil)
	if err != nil {
		return FileInfo{}, err
	}
	fields := nulSeparated(reply.payload)
	attributes := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		attributes[fields[i]] = fields[i+1]
	}
	size, err := strconv.ParseInt(attributes["st_size"], 10, 64)
	if err != nil {
		return FileInfo{}, fmt.Errorf("%w: stat %s without st_size", ios.ErrProtocol, path)
	}
	info := FileInfo{Size: size, IsDir: attributes["st_ifmt"] == "S_IFDIR"}
	if nanos, err := strconv.ParseInt(attributes["st_mtime"], 10, 64); err == nil && nanos > 0 {
		info.ModTime = time.Unix(0, nanos)
	}
	return info, nil
}

func (c *Client) Remove(ctx context.Context, path string) error {
	_, err := c.request(ctx, opRemovePath, []byte(path), nil)
	return err
}

// RemoveAll removes path and everything below it.
func (c *Client) RemoveAll(ctx context.Context, path string) error {
	_, err := c.request(ctx, opRemoveAll, []byte(path), nil)
	return err
}

func (c *Client) MakeDir(ctx context.Context, path string) error {
	_, err := c.request(ctx, opMakeDir, []byte(path), nil)
	return err
}

// File shares its Client's connection, so the two take turns.
type File struct {
	client *Client
	fd     uint64
}

func (c *Client) Open(ctx context.Context, path string, mode Mode) (*File, error) {
	reply, err := c.request(ctx, opFileOpen, append(le64(uint64(mode)), path...), nil)
	if err != nil {
		return nil, err
	}
	if reply.op != opFileOpenRes || len(reply.header) < 8 {
		c.torn = true
		return nil, fmt.Errorf("%w: open %s without a descriptor", ios.ErrProtocol, path)
	}
	c.open++
	return &File{client: c, fd: binary.LittleEndian.Uint64(reply.header)}, nil
}

func (f *File) Read(ctx context.Context, p []byte) (int, error) {
	n := min(len(p), MaxTransfer)
	reply, err := f.client.request(ctx, opFileRead, append(le64(f.fd), le64(uint64(n))...), nil)
	switch {
	case errors.Is(err, ErrEndOfData):
		return 0, io.EOF
	case err != nil:
		return 0, err
	case len(reply.payload) > n:
		f.client.torn = true
		return 0, fmt.Errorf("%w: read returned %d bytes for %d", ios.ErrProtocol, len(reply.payload), n)
	case len(reply.payload) == 0:
		return 0, io.EOF
	}
	return copy(p, reply.payload), nil
}

func (f *File) Write(ctx context.Context, p []byte) error {
	for len(p) > 0 {
		chunk := p[:min(len(p), MaxTransfer)]
		if _, err := f.client.request(ctx, opFileWrite, le64(f.fd), chunk); err != nil {
			return err
		}
		p = p[len(chunk):]
	}
	return nil
}

func (f *File) Seek(ctx context.Context, offset int64) error {
	header := append(le64(f.fd), le64(0)...) // SEEK_SET
	_, err := f.client.request(ctx, opFileSeek, append(header, le64(uint64(offset))...), nil)
	return err
}

// Lock fails with ErrOpWouldBlock while another holds it.
func (f *File) Lock(ctx context.Context, op Lock) error {
	_, err := f.client.request(ctx, opFileLock, append(le64(f.fd), le64(uint64(op))...), nil)
	return err
}

func (f *File) Close(ctx context.Context) error {
	_, err := f.client.request(ctx, opFileClose, le64(f.fd), nil)
	if err == nil {
		f.client.open--
	}
	return err
}

type packet struct {
	op      uint64
	header  []byte
	payload []byte
}

func (c *Client) request(ctx context.Context, op uint64, header, payload []byte) (packet, error) {
	if c.torn {
		return packet{}, errTorn
	}
	var reply packet
	torn, err := ios.Guard(ctx, c.conn, func() (err error) {
		reply, err = c.exchange(op, header, payload)
		return err
	})
	if torn || err != nil {
		c.torn = true
		return packet{}, err
	}
	if reply.op == opStatus {
		if len(reply.header) < 8 {
			c.torn = true
			return packet{}, fmt.Errorf("%w: status without a code", ios.ErrProtocol)
		}
		if code := Error(binary.LittleEndian.Uint64(reply.header)); code != 0 {
			return packet{}, code
		}
	}
	return reply, nil
}

func (c *Client) exchange(op uint64, header, payload []byte) (packet, error) {
	c.packet++
	out := make([]byte, headerSize, headerSize+len(header)+len(payload))
	binary.LittleEndian.PutUint64(out[0:], magic)
	binary.LittleEndian.PutUint64(out[8:], uint64(headerSize+len(header)+len(payload)))
	binary.LittleEndian.PutUint64(out[16:], uint64(headerSize+len(header)))
	binary.LittleEndian.PutUint64(out[24:], c.packet)
	binary.LittleEndian.PutUint64(out[32:], op)
	out = append(append(out, header...), payload...)
	if _, err := c.conn.Write(out); err != nil {
		return packet{}, err
	}
	return readPacket(c.conn)
}

func readPacket(r io.Reader) (packet, error) {
	var head [headerSize]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return packet{}, err
	}
	le := binary.LittleEndian
	total, headerLen := le.Uint64(head[8:]), le.Uint64(head[16:])
	if le.Uint64(head[0:]) != magic || headerLen < headerSize || total < headerLen || total > maxPacket {
		return packet{}, fmt.Errorf("%w: afc packet header %x", ios.ErrProtocol, head)
	}
	body := make([]byte, total-headerSize)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return packet{}, err
	}
	split := headerLen - headerSize
	return packet{op: le.Uint64(head[32:]), header: body[:split], payload: body[split:]}, nil
}

func nulSeparated(payload []byte) []string {
	var fields []string
	for field := range bytes.SplitSeq(payload, []byte{0}) {
		if len(field) > 0 {
			fields = append(fields, string(field))
		}
	}
	return fields
}

func le64(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }
