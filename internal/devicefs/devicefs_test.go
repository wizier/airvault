package devicefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// fakeFile is a native device file that counts its round trips.
type fakeFile struct {
	*bytes.Reader
	size        int64
	reads       int
	largestRead int
	seeks       int
}

func newFakeFile(data string, size int64) *fakeFile {
	return &fakeFile{Reader: bytes.NewReader([]byte(data)), size: size}
}

func (f *fakeFile) Read(buffer []byte) (int, error) {
	f.reads++
	f.largestRead = max(f.largestRead, len(buffer))
	return f.Reader.Read(buffer)
}

func (f *fakeFile) SeekTo(offset int64) error {
	f.seeks++
	_, err := f.Reader.Seek(offset, io.SeekStart)
	return err
}

func (f *fakeFile) Size() int64        { return f.size }
func (f *fakeFile) ModTime() time.Time { return time.Time{} }
func (f *fakeFile) Close() error       { return nil }

func managerForFile(file engine.AFCFile) *Manager {
	return New(fakeOpener{open: func(context.Context) engine.AFCSession {
		return &fakeSession{file: file}
	}})
}

func openTestFile(t *testing.T, native engine.AFCFile) *File {
	t.Helper()
	path, err := ParsePath("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	session, err := managerForFile(native).Open(context.Background(), "device", Media())
	if err != nil {
		t.Fatal(err)
	}
	file, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestReadStopsAtOpeningSize(t *testing.T) {
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
			native := newFakeFile(test.data, test.size)
			read, err := io.ReadAll(openTestFile(t, native))
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if got, want := string(read), test.data[:min(len(test.data), int(test.size))]; got != want {
				t.Fatalf("read %q, want %q", got, want)
			}
			if test.wantErr == nil && native.reads != 1 {
				t.Fatalf("native reads = %d, want 1", native.reads)
			}
		})
	}
}

// http.ServeContent sizes the file with two seeks and copies in 32 KiB reads.
// Neither may cost a device round trip: a whole file takes one native read per
// MiB and no seek, a range exactly one seek.
func TestServeContentReadsAheadAndSeeksLazily(t *testing.T) {
	data := make([]byte, 3*readAheadSize+7)
	for i := range data {
		data[i] = byte(i % 251)
	}
	serve := func(byteRange string) (*fakeFile, *httptest.ResponseRecorder) {
		native := newFakeFile(string(data), int64(len(data)))
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		if byteRange != "" {
			request.Header.Set("Range", byteRange)
		}
		recorder := httptest.NewRecorder()
		// Set as the handler does; ServeContent would otherwise sniff and rewind.
		recorder.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(recorder, request, "file.bin", time.Time{}, openTestFile(t, native))
		return native, recorder
	}

	native, recorder := serve("")
	if !bytes.Equal(recorder.Body.Bytes(), data) {
		t.Fatalf("full body differs (%d bytes)", recorder.Body.Len())
	}
	if native.seeks != 0 || native.reads != 4 || native.largestRead > readAheadSize {
		t.Fatalf("full: seeks = %d, reads = %d, largest read = %d; want 0, 4, <= %d",
			native.seeks, native.reads, native.largestRead, readAheadSize)
	}

	const start = readAheadSize + 5
	native, recorder = serve(fmt.Sprintf("bytes=%d-", start))
	if recorder.Code != http.StatusPartialContent || !bytes.Equal(recorder.Body.Bytes(), data[start:]) {
		t.Fatalf("range: status %d, %d bytes", recorder.Code, recorder.Body.Len())
	}
	if native.seeks != 1 {
		t.Fatalf("range: seeks = %d, want 1", native.seeks)
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
