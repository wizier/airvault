package ios

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"howett.net/plist"
)

const OSTraceService = "com.apple.os_trace_relay"

const maxLogRecord = 1 << 20

type LogRecord struct {
	Time      time.Time
	Level     string // notice | info | debug | error | fault
	PID       uint32
	Image     string // the emitting image's name
	Message   string
	Subsystem string // empty without a label
	Category  string
}

type OSTrace struct {
	conn   net.Conn
	reader *bufio.Reader
}

func StartOSTrace(ctx context.Context, conn net.Conn) (*OSTrace, error) {
	reader := bufio.NewReader(conn)
	var reply struct {
		Status string `plist:"Status"`
	}
	_, err := Guard(ctx, conn, func() error {
		request := map[string]any{"Request": "StartActivity", "Pid": -1, "MessageFilter": 65535, "StreamFlags": 60}
		if err := NewPlistConn(conn, plist.BinaryFormat).Send(request); err != nil {
			return err
		}
		// One byte precedes the framed reply.
		if _, err := reader.ReadByte(); err != nil {
			return err
		}
		body, err := ReadFrame(reader)
		if err == nil {
			err = replyError(body)
		}
		if err == nil {
			err = decodePlist(body, &reply)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("start log stream: %w", err)
	}
	if reply.Status != "RequestSuccessful" {
		return nil, fmt.Errorf("start log stream: %w: status %q", ErrProtocol, reply.Status)
	}
	return &OSTrace{conn: conn, reader: reader}, nil
}

func (t *OSTrace) Close() error { return t.conn.Close() }

func (t *OSTrace) Next(ctx context.Context) (LogRecord, error) {
	var packet []byte
	_, err := Guard(ctx, t.conn, func() error {
		var header [5]byte
		if _, err := io.ReadFull(t.reader, header[:]); err != nil {
			return err
		}
		size := binary.LittleEndian.Uint32(header[1:])
		if header[0] != 0x02 || size > maxLogRecord {
			return fmt.Errorf("%w: log record marker %#x, %d bytes", ErrProtocol, header[0], size)
		}
		packet = make([]byte, size)
		_, err := io.ReadFull(t.reader, packet)
		return noEOF(err)
	})
	if err != nil {
		return LogRecord{}, err
	}
	return parseLogRecord(packet)
}

// A record is a 129-byte header, then NUL-terminated strings: source file,
// image, message and, when labelled, subsystem and category.
const logHeaderSize = 129

var logLevels = map[byte]string{0x00: "notice", 0x01: "info", 0x02: "debug", 0x10: "error", 0x11: "fault"}

func parseLogRecord(packet []byte) (LogRecord, error) {
	if len(packet) < logHeaderSize {
		return LogRecord{}, fmt.Errorf("%w: log record of %d bytes", ErrProtocol, len(packet))
	}
	le := binary.LittleEndian
	record := LogRecord{
		PID:  le.Uint32(packet[9:]),
		Time: time.Unix(int64(le.Uint32(packet[55:])), int64(le.Uint32(packet[63:]))*int64(time.Microsecond)),
	}
	level, known := logLevels[packet[68]]
	if !known {
		level = "notice"
	}
	record.Level = level
	sizes := []int{int(le.Uint16(packet[107:])), int(le.Uint16(packet[109:])), int(le.Uint32(packet[117:])), int(le.Uint32(packet[121:]))}

	_, rest, found := bytes.Cut(packet[logHeaderSize:], []byte{0}) // past the file name
	if !found {
		return LogRecord{}, fmt.Errorf("%w: unterminated log file name", ErrProtocol)
	}
	fields := []*string{&record.Image, &record.Message, &record.Subsystem, &record.Category}
	for i, field := range fields {
		if i == 2 && (sizes[2] == 0 || sizes[3] == 0 || len(rest) == 0) {
			break // unlabelled
		}
		if sizes[i] > len(rest) {
			return LogRecord{}, fmt.Errorf("%w: log string of %d bytes past the record", ErrProtocol, sizes[i])
		}
		*field = string(bytes.TrimSuffix(rest[:sizes[i]], []byte{0}))
		rest = rest[sizes[i]:]
	}
	return record, nil
}
