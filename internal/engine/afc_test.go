package engine

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
	"github.com/wizier/airvault/internal/ios/iostest"
)

func phoneWithMedia(t *testing.T) (*testPhone, *iostest.FS) {
	t.Helper()
	p := newTestPhone(t)
	media := iostest.NewFS()
	media.WriteFile("DCIM/100APPLE/IMG_0001.JPG", []byte("jpeg"))
	media.WriteFile("DCIM/100APPLE/IMG_0002.MOV", []byte(strings.Repeat("v", 3<<20)))
	p.phone.Handle(afc.Service, media.Serve)
	return p, media
}

func TestAFCSession(t *testing.T) {
	p, _ := phoneWithMedia(t)
	session, err := p.engine.OpenAFC(context.Background(), p.udid, AFCMedia, "")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if names, err := session.List("/DCIM/100APPLE"); err != nil || !slices.Contains(names, "IMG_0001.JPG") {
		t.Fatalf("List = %v, %v", names, err)
	}
	if entry, err := session.Stat("/DCIM/100APPLE/IMG_0001.JPG"); err != nil || entry.Size != 4 || entry.IsDir || entry.Modified == 0 {
		t.Fatalf("Stat = %+v, %v", entry, err)
	}
	// A device answer leaves the session working.
	_, err = session.Stat("/missing.jpg")
	if kindOf(err) != ErrorNotFound || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v, want ErrorNotFound", err)
	}
	if _, err := session.ReadSmall("/DCIM"); err == nil {
		t.Fatal("ReadSmall of a directory succeeded")
	}
	if _, err := session.ReadSmall("/DCIM/100APPLE/IMG_0002.MOV"); kindOf(err) != ErrorInternal {
		t.Fatalf("ReadSmall over the cap: %v, want ErrorInternal", err)
	}
	if data, err := session.ReadSmall("/DCIM/100APPLE/IMG_0001.JPG"); err != nil || string(data) != "jpeg" {
		t.Fatalf("ReadSmall = %q, %v", data, err)
	}
	if err := session.Remove("/DCIM/100APPLE/IMG_0001.JPG"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.List("relative"); kindOf(err) != ErrorInvalidArgument {
		t.Fatalf("relative path: %v", err)
	}
}

// A file reads to io.EOF, seeks, and on Close hands its connection back:
// the next session reuses it without a new handshake.
func TestAFCFileAndPooling(t *testing.T) {
	p, media := phoneWithMedia(t)
	ctx := context.Background()
	session, err := p.engine.OpenAFC(ctx, p.udid, AFCMedia, "")
	if err != nil {
		t.Fatal(err)
	}
	file, err := session.Open("/DCIM/100APPLE/IMG_0002.MOV")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.List("/"); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("session after Open = %v, want io.ErrClosedPipe", err)
	}
	if file.Size() != 3<<20 || file.ModTime().IsZero() {
		t.Fatalf("file size %d, modified %v", file.Size(), file.ModTime())
	}
	if err := file.SeekTo(3<<20 - 2); err != nil {
		t.Fatal(err)
	}
	if data, err := io.ReadAll(file); err != nil || string(data) != "vv" {
		t.Fatalf("read after seek = %q, %v", data, err)
	}
	_ = file.Close()
	if media.OpenHandles() != 0 {
		t.Fatal("the descriptor was left open")
	}

	again, err := p.engine.OpenAFC(ctx, p.udid, AFCMedia, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.List("/"); err != nil {
		t.Fatal(err)
	}
	_ = again.Close()
	if dials := p.phone.Dialed(afc.Service); dials != 1 {
		t.Fatalf("AFC connections = %d, want the pooled one reused", dials)
	}
}

// Close interrupts a read the phone stopped answering; the torn connection
// is dropped, not pooled.
func TestAFCCloseInterruptsARead(t *testing.T) {
	p, media := phoneWithMedia(t)
	media.Hang("DCIM/100APPLE/IMG_0002.MOV")
	session, err := p.engine.OpenAFC(context.Background(), p.udid, AFCMedia, "")
	if err != nil {
		t.Fatal(err)
	}
	file, err := session.Open("/DCIM/100APPLE/IMG_0002.MOV")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var readErr error
	wg.Go(func() { _, readErr = file.Read(make([]byte, 16)) })
	time.Sleep(20 * time.Millisecond)
	_ = file.Close()
	wg.Wait()
	if !errors.Is(readErr, io.ErrClosedPipe) {
		t.Fatalf("interrupted read = %v, want io.ErrClosedPipe", readErr)
	}
	next, err := p.engine.OpenAFC(context.Background(), p.udid, AFCMedia, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = next.Close()
	if dials := p.phone.Dialed(afc.Service); dials != 2 {
		t.Fatalf("AFC connections = %d, want a fresh one after the torn one", dials)
	}
}

// A cancelled Open context outranks the error it caused.
func TestAFCOwnerContextWins(t *testing.T) {
	p, _ := phoneWithMedia(t)
	ctx, cancel := context.WithCancel(context.Background())
	session, err := p.engine.OpenAFC(ctx, p.udid, AFCMedia, "")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	cancel()
	if _, err := session.List("/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("List after cancel = %v, want context.Canceled", err)
	}
}

func TestAFCPoolRules(t *testing.T) {
	p, _ := phoneWithMedia(t)
	pool := p.engine.afc
	clock := time.Unix(1_790_000_000, 0)
	pool.now = func() time.Time { return clock }
	ctx := context.Background()
	open := func() {
		t.Helper()
		session, err := p.engine.OpenAFC(ctx, p.udid, AFCMedia, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := session.List("/"); err != nil {
			t.Fatal(err)
		}
		_ = session.Close()
	}
	open()
	clock = clock.Add(time.Second)
	open() // fresh enough to reuse as is
	clock = clock.Add(10 * time.Second)
	open() // validated with a stat, then reused
	if dials := p.phone.Dialed(afc.Service); dials != 1 {
		t.Fatalf("dials = %d, want 1", dials)
	}
	clock = clock.Add(afcIdleTimeout)
	open() // idle too long
	clock = clock.Add(time.Second)
	pool.forget(string(p.udid))
	open() // forgotten
	if dials := p.phone.Dialed(afc.Service); dials != 3 {
		t.Fatalf("dials = %d, want 3", dials)
	}
	// A connection made over USB is not reused once the device is on Wi-Fi.
	key := afcKey{udid: string(p.udid), source: AFCMedia}
	if _, ok := pool.take(key, afcTransport{connection: ios.ConnectionNetwork}); ok {
		t.Fatal("a USB connection was handed out for Wi-Fi")
	}
}

func TestAppDocuments(t *testing.T) {
	p := newTestPhone(t)
	documents := iostest.NewFS()
	documents.WriteFile("Documents/notes.txt", []byte("hello"))
	p.phone.Handle(ios.HouseArrestService, iostest.HouseArrest(map[string]*iostest.FS{"com.example.notes": documents}))
	ctx := context.Background()
	session, err := p.engine.OpenAFC(ctx, p.udid, AFCAppDocuments, "com.example.notes")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if data, err := session.ReadSmall("/Documents/notes.txt"); err != nil || string(data) != "hello" {
		t.Fatalf("ReadSmall = %q, %v", data, err)
	}
	if _, err := p.engine.OpenAFC(ctx, p.udid, AFCAppDocuments, "com.example.unknown"); err == nil {
		t.Fatal("vending an unknown app succeeded")
	}
	for _, bad := range []struct {
		source AFCSource
		bundle string
	}{{AFCMedia, "com.example.notes"}, {AFCAppDocuments, ""}, {7, "x"}} {
		if _, err := p.engine.OpenAFC(ctx, p.udid, bad.source, bad.bundle); kindOf(err) != ErrorInvalidArgument {
			t.Errorf("OpenAFC(%d, %q) = %v, want ErrorInvalidArgument", bad.source, bad.bundle, err)
		}
	}
}
