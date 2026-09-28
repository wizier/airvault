package ios

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// logPacket builds a relayed log record: the fixed header, then the file
// name and the given NUL-terminated strings.
func logPacket(level byte, strings ...string) []byte {
	header := make([]byte, logHeaderSize)
	le := binary.LittleEndian
	le.PutUint32(header[9:], 4242)
	le.PutUint32(header[55:], 1_790_000_000)
	le.PutUint32(header[63:], 123_456) // microseconds
	header[68] = level
	sizes := make([]int, 4)
	for i, s := range strings {
		sizes[i] = len(s) + 1
	}
	le.PutUint16(header[107:], uint16(sizes[0]))
	le.PutUint16(header[109:], uint16(sizes[1]))
	le.PutUint32(header[117:], uint32(sizes[2]))
	le.PutUint32(header[121:], uint32(sizes[3]))
	packet := append(header, "Source.m\x00"...)
	for _, s := range strings {
		packet = append(append(packet, s...), 0)
	}
	return packet
}

func TestParseLogRecord(t *testing.T) {
	record, err := parseLogRecord(logPacket(0x10, "SpringBoard", "hello", "com.apple.ui", "touch"))
	if err != nil {
		t.Fatal(err)
	}
	want := LogRecord{
		Time:      time.Unix(1_790_000_000, 123_456_000),
		Level:     "error",
		PID:       4242,
		Image:     "SpringBoard",
		Message:   "hello",
		Subsystem: "com.apple.ui",
		Category:  "touch",
	}
	if !record.Time.Equal(want.Time) {
		t.Fatalf("time = %v, want %v (microseconds, not nanoseconds)", record.Time, want.Time)
	}
	record.Time = want.Time
	if record != want {
		t.Fatalf("record = %+v\nwant %+v", record, want)
	}

	unlabelled, err := parseLogRecord(logPacket(0x01, "kernel", "boot"))
	if err != nil || unlabelled.Subsystem != "" || unlabelled.Level != "info" {
		t.Fatalf("unlabelled record = %+v, %v", unlabelled, err)
	}
	// A level a newer iOS might add must not break the console.
	if future, err := parseLogRecord(logPacket(0x42, "app", "x")); err != nil || future.Level != "notice" {
		t.Fatalf("unknown level = %+v, %v", future, err)
	}
}

func TestParseLogRecordRejectsBadSizes(t *testing.T) {
	truncated := logPacket(0, "SpringBoard", "hello")
	truncated = truncated[:len(truncated)-3]
	noFileEnd := make([]byte, logHeaderSize+4)
	for i := logHeaderSize; i < len(noFileEnd); i++ {
		noFileEnd[i] = 'x'
	}
	for name, packet := range map[string][]byte{
		"short":       make([]byte, logHeaderSize-1),
		"truncated":   truncated,
		"no file end": noFileEnd,
	} {
		if _, err := parseLogRecord(packet); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v, want ErrProtocol", name, err)
		}
	}
}

func FuzzParseLogRecord(f *testing.F) {
	f.Add(logPacket(0, "SpringBoard", "hello", "sub", "cat"))
	f.Add(make([]byte, logHeaderSize))
	f.Fuzz(func(t *testing.T, packet []byte) {
		_, _ = parseLogRecord(packet) // must never panic
	})
}
