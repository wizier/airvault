package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/iostest"
)

// A stream fails once, then reports itself closed; a cancelled one reports
// the context's error and a closed one io.ErrClosedPipe.
func TestPullLifecycle(t *testing.T) {
	values := []error{nil, errors.New("device went away")}
	closed := 0
	stream := newPull(context.Background(), func(context.Context) (int, error) {
		err := values[0]
		values = values[1:]
		return 7, err
	}, func() { closed++ })
	if value, err := stream.Next(); value != 7 || err != nil {
		t.Fatalf("first Next = %d, %v", value, err)
	}
	if _, err := stream.Next(); err == nil || errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failing Next = %v, want the failure", err)
	}
	if _, err := stream.Next(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Next after a failure = %v, want io.ErrClosedPipe", err)
	}
	_ = stream.Close()
	_ = stream.Close()
	if closed != 1 {
		t.Fatalf("close ran %d times", closed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	blocked := newPull(ctx, func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}, nil)
	cancel()
	if _, err := blocked.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Next = %v, want context.Canceled", err)
	}

	waiting := newPull(context.Background(), func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}, nil)
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = waiting.Close()
	}()
	if _, err := waiting.Next(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Next interrupted by Close = %v, want io.ErrClosedPipe", err)
	}
}

// The watcher publishes the muxer's state, a change, one down state while the
// muxer is gone, and the state again once it is back.
func TestPresenceWatcher(t *testing.T) {
	p := newTestPhone(t)
	watcher, err := p.engine.OpenPresenceWatcher(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	next := func() PresenceState {
		t.Helper()
		state, err := watcher.Next()
		if err != nil {
			t.Fatal(err)
		}
		return state
	}

	if state := next(); !state.MuxUp || len(state.Devices) != 1 {
		t.Fatalf("first state = %+v", state)
	}
	_, identity := iostest.NewPairing(t)
	p.muxer.Attach(iostest.NewDevice("SECOND", identity), ios.ConnectionNetwork)
	if state := next(); len(state.Devices) != 2 {
		t.Fatalf("after attach = %+v", state)
	}
	p.muxer.SetDown(true)
	if state := next(); state.MuxUp || len(state.Devices) != 0 {
		t.Fatalf("muxer down = %+v", state)
	}
	p.muxer.SetDown(false)
	if state := next(); !state.MuxUp || len(state.Devices) != 2 {
		t.Fatalf("muxer back = %+v", state)
	}
	_ = watcher.Close()
	if _, err := watcher.Next(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Next after Close = %v", err)
	}
}

func TestLockObserver(t *testing.T) {
	p := newTestPhone(t)
	observed := make(chan []string, 1)
	p.phone.Handle(ios.NotificationProxyService, func(raw net.Conn) {
		conn := ios.NewPlistConn(raw, plist.XMLFormat)
		var names []string
		for len(names) < 2 {
			var request map[string]any
			if err := conn.Recv(&request); err != nil || request["Command"] != "ObserveNotification" {
				return
			}
			names = append(names, request["Name"].(string))
		}
		observed <- names
		for _, name := range []string{lockStateChanged, "com.apple.other", lockComplete} {
			_ = conn.Send(map[string]any{"Command": "RelayNotification", "Name": name})
		}
		_ = conn.Send(map[string]any{"Command": "ProxyDeath"})
	})
	stream, err := p.engine.OpenLockObserver(context.Background(), p.udid)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if names := <-observed; names[0] != lockStateChanged || names[1] != lockComplete {
		t.Fatalf("observed %v", names)
	}
	for _, want := range []ScreenLockSignal{ScreenLockChanged, ScreenLockComplete} {
		if signal, err := stream.Next(); signal != want || err != nil {
			t.Fatalf("Next = %v, %v; want %v", signal, err, want)
		}
	}
	if _, err := stream.Next(); err == nil || errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("ProxyDeath = %v, want an error", err)
	}
	if _, err := stream.Next(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("after ProxyDeath = %v, want io.ErrClosedPipe", err)
	}
}

func TestConsole(t *testing.T) {
	p := newTestPhone(t)
	p.phone.Handle(ios.OSTraceService, func(raw net.Conn) {
		var request map[string]any
		if err := ios.NewPlistConn(raw, plist.BinaryFormat).Recv(&request); err != nil || request["Request"] != "StartActivity" {
			return
		}
		body, _ := plist.Marshal(map[string]any{"Status": "RequestSuccessful"}, plist.BinaryFormat)
		_, _ = raw.Write([]byte{0})
		_ = ios.WriteFrame(raw, body)
		_, _ = raw.Write(logRecord(4242, 1_790_000_000, 250_000, 0x11, "backupd", "done"))
	})
	console, err := p.engine.OpenConsole(context.Background(), p.udid)
	if err != nil {
		t.Fatal(err)
	}
	defer console.Close()
	line, err := console.Next()
	if err != nil {
		t.Fatal(err)
	}
	want := ConsoleLine{
		Timestamp: time.Unix(1_790_000_000, 0).UTC().Format("15:04:05") + ".250",
		Level:     "fault", Pid: 4242, Image: "backupd", Message: "done",
	}
	if line != want {
		t.Fatalf("line = %+v\nwant %+v", line, want)
	}
	if _, err := console.Next(); err == nil {
		t.Fatal("Next after the phone hung up succeeded")
	}
	if _, err := console.Next(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("second Next after the failure = %v", err)
	}
}

// logRecord frames one unlabelled os_trace record.
func logRecord(pid, seconds, microseconds uint32, level byte, image, message string) []byte {
	header := make([]byte, 129)
	le := binary.LittleEndian
	le.PutUint32(header[9:], pid)
	le.PutUint32(header[55:], seconds)
	le.PutUint32(header[63:], microseconds)
	header[68] = level
	le.PutUint16(header[107:], uint16(len(image)+1))
	le.PutUint16(header[109:], uint16(len(message)+1))
	packet := append(header, "file.c\x00"...)
	packet = append(append(packet, image...), 0)
	packet = append(append(packet, message...), 0)
	frame := make([]byte, 5, 5+len(packet))
	frame[0] = 0x02
	le.PutUint32(frame[1:], uint32(len(packet)))
	return append(frame, packet...)
}
