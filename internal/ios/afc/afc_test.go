package afc_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/ios/afc"
	"github.com/wizier/airvault/internal/ios/iostest"
)

func connect(t *testing.T, files *iostest.FS) *afc.Client {
	t.Helper()
	client, device := net.Pipe()
	go files.Serve(device)
	t.Cleanup(func() { _ = client.Close() })
	return afc.New(client)
}

func TestClientFileOperations(t *testing.T) {
	files := iostest.NewFS()
	files.WriteFile("DCIM/100APPLE/IMG_0001.JPG", []byte("jpeg bytes"))
	client := connect(t, files)
	ctx := context.Background()

	names, err := client.List(ctx, "/DCIM/100APPLE")
	if err != nil || !slices.Equal(names, []string{".", "..", "IMG_0001.JPG"}) {
		t.Fatalf("List = %v, %v", names, err)
	}
	info, err := client.Stat(ctx, "/DCIM/100APPLE/IMG_0001.JPG")
	if err != nil || info.Size != 10 || info.IsDir || info.ModTime.IsZero() {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if dir, err := client.Stat(ctx, "/DCIM"); err != nil || !dir.IsDir {
		t.Fatalf("Stat dir = %+v, %v", dir, err)
	}
	_, err = client.Stat(ctx, "/missing")
	if !errors.Is(err, afc.ErrObjectNotFound) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v, want ErrObjectNotFound and fs.ErrNotExist", err)
	}
	if !client.Idle() {
		t.Fatal("a status error must leave the client usable")
	}

	file, err := client.Open(ctx, "/DCIM/100APPLE/IMG_0001.JPG", afc.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if client.Idle() {
		t.Fatal("a client with an open descriptor is not idle")
	}
	if err := file.Seek(ctx, 5); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	if n, err := file.Read(ctx, buffer); err != nil || string(buffer[:n]) != "bytes" {
		t.Fatalf("Read after seek = %q, %v", buffer[:n], err)
	}
	if _, err := file.Read(ctx, buffer); err != io.EOF {
		t.Fatalf("Read at the end = %v, want io.EOF", err)
	}
	if err := file.Close(ctx); err != nil || !client.Idle() {
		t.Fatalf("Close = %v, idle %v", err, client.Idle())
	}

	out, err := client.Open(ctx, "PublicStaging/new.ipa", afc.WriteOnly)
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Write(ctx, make([]byte, afc.MaxTransfer+10)); err != nil {
		t.Fatal(err)
	}
	_ = out.Close(ctx)
	if data, _ := files.ReadFile("PublicStaging/new.ipa"); len(data) != afc.MaxTransfer+10 {
		t.Fatalf("written %d bytes", len(data))
	}
	if err := client.Remove(ctx, "PublicStaging/new.ipa"); err != nil || files.Exists("PublicStaging/new.ipa") {
		t.Fatalf("Remove = %v", err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	files := iostest.NewFS()
	files.WriteFile("com.apple.itunes.lock_sync", nil)
	files.Lock("com.apple.itunes.lock_sync", true) // another host holds it
	client := connect(t, files)
	ctx := context.Background()
	file, err := client.Open(ctx, "/com.apple.itunes.lock_sync", afc.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Lock(ctx, afc.LockExclusive); !errors.Is(err, afc.ErrOpWouldBlock) {
		t.Fatalf("held lock: %v, want ErrOpWouldBlock", err)
	}
	files.Lock("com.apple.itunes.lock_sync", false)
	if err := file.Lock(ctx, afc.LockExclusive); err != nil {
		t.Fatal(err)
	}
	if err := file.Lock(ctx, afc.LockRelease); err != nil {
		t.Fatal(err)
	}
}

// A read the context interrupts tears the connection, which then refuses use.
func TestInterruptedReadTearsTheClient(t *testing.T) {
	files := iostest.NewFS()
	files.WriteFile("slow.mov", []byte("frames"))
	files.Hang("slow.mov")
	client := connect(t, files)
	file, err := client.Open(context.Background(), "/slow.mov", afc.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := file.Read(ctx, make([]byte, 8)); err == nil {
		t.Fatal("a hung read returned")
	}
	if !client.Torn() {
		t.Fatal("an interrupted read must tear the client")
	}
	if _, err := client.Stat(context.Background(), "/"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("use after tear = %v, want net.ErrClosed", err)
	}
}
