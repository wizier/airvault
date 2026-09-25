package devicefs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wizier/airvault/internal/engine"
)

type fakeOpener struct {
	open func(context.Context) engine.AFCSession
}

func (f fakeOpener) OpenAFC(ctx context.Context, _ engine.DeviceID, _ engine.AFCSource, _ string) (engine.AFCSession, error) {
	return f.open(ctx), nil
}

type fakeSession struct {
	file engine.AFCFile
}

func (s *fakeSession) List(string) ([]string, error) { return nil, nil }
func (s *fakeSession) Stat(string) (engine.AFCEntry, error) {
	return engine.AFCEntry{Size: 1}, nil
}
func (s *fakeSession) Remove(string) error                 { return nil }
func (s *fakeSession) Open(string) (engine.AFCFile, error) { return s.file, nil }
func (s *fakeSession) ReadSmall(string) ([]byte, error)    { return nil, nil }
func (s *fakeSession) Close() error                        { return nil }

type fakeFile struct {
	*bytes.Reader
	size int64
}

func newFakeFile(data string, size int64) *fakeFile {
	return &fakeFile{Reader: bytes.NewReader([]byte(data)), size: size}
}

func (f *fakeFile) Size() int64  { return f.size }
func (f *fakeFile) Close() error { return nil }

func managerForFile(file engine.AFCFile) *Manager {
	return New(fakeOpener{open: func(context.Context) engine.AFCSession {
		return &fakeSession{file: file}
	}})
}

func copyWithManager(
	ctx context.Context,
	manager *Manager,
	path Path,
	destination io.Writer,
) error {
	session, err := manager.Open(ctx, "device", Media())
	if err != nil {
		return err
	}
	defer session.Close()
	file, err := session.OpenFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.CopyTo(ctx, destination, nil)
}

func TestCopyVerifiesOpeningSize(t *testing.T) {
	path, err := ParsePath("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		data    string
		size    int64
		wantErr error
	}{
		{name: "exact", data: "abc", size: 3},
		{name: "short", data: "ab", size: 3, wantErr: io.ErrUnexpectedEOF},
		// The opening size defines the HTTP snapshot. Appends after stat are not
		// part of this download and must not trigger a probe past EOF.
		{name: "grew", data: "abcd", size: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			var destination bytes.Buffer
			err := copyWithManager(
				context.Background(), managerForFile(newFakeFile(test.data, test.size)), path, &destination,
			)
			switch test.wantErr {
			case nil:
				if err != nil {
					t.Fatal(err)
				}
			default:
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
			}
			if got, want := destination.String(), test.data[:min(len(test.data), int(test.size))]; got != want {
				t.Fatalf("copied %q, want %q", got, want)
			}
		})
	}
}

type blockingFile struct {
	started   chan struct{}
	closed    chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
}

func newBlockingFile() *blockingFile {
	return &blockingFile{started: make(chan struct{}), closed: make(chan struct{})}
}

func (f *blockingFile) Read([]byte) (int, error) {
	f.readOnce.Do(func() { close(f.started) })
	<-f.closed
	return 0, io.ErrClosedPipe
}

func (f *blockingFile) Size() int64 { return 1 << 20 }
func (f *blockingFile) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func TestCopyCancellationClosesInFlightFile(t *testing.T) {
	file := newBlockingFile()
	path, err := ParsePath("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- copyWithManager(ctx, managerForFile(file), path, io.Discard)
	}()
	select {
	case <-file.started:
	case <-time.After(time.Second):
		t.Fatal("copy did not start reading")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("copy error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled copy stayed blocked")
	}
}

func TestManagerLimitsConcurrentSessionsPerDevice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		opened := 0
		opener := fakeOpener{open: func(context.Context) engine.AFCSession {
			mu.Lock()
			opened++
			mu.Unlock()
			return &fakeSession{}
		}}
		manager := New(opener)
		var sessions []*Session
		for range maxConcurrentSessions {
			session, err := manager.Open(context.Background(), "device", Media())
			if err != nil {
				t.Fatal(err)
			}
			sessions = append(sessions, session)
		}
		openedFourth := make(chan *Session, 1)
		go func() {
			session, _ := manager.Open(context.Background(), "device", Media())
			openedFourth <- session
		}()
		synctest.Wait()
		select {
		case <-openedFourth:
			t.Fatal("fourth session bypassed the limiter")
		default:
		}
		_ = sessions[0].Close()
		select {
		case session := <-openedFourth:
			if session == nil {
				t.Fatal("fourth session did not open")
			}
			_ = session.Close()
		case <-time.After(time.Second):
			t.Fatal("fourth session stayed blocked")
		}
		for _, session := range sessions[1:] {
			_ = session.Close()
		}
		mu.Lock()
		defer mu.Unlock()
		if opened != maxConcurrentSessions+1 {
			t.Fatalf("opened = %d", opened)
		}
	})
}
