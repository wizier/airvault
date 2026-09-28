package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

var testModTime = time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)

// testDownload serves an in-memory body as an open device file.
type testDownload struct {
	*strings.Reader
}

func (testDownload) ModTime() time.Time { return testModTime }
func (testDownload) Close()             {}

func serveTestDownload(request *http.Request, download deviceDownload) (*httptest.ResponseRecorder, error) {
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(request, recorder)
	err := serveDownload(c, download, path.Base(c.QueryParam("path")))
	return recorder, err
}

func TestServeDownloadWritesCompleteAttachment(t *testing.T) {
	const payload = "phone file contents"
	request := httptest.NewRequest(http.MethodGet, "/download?path=DCIM/camera%20file.heic", nil)
	recorder, err := serveTestDownload(request, testDownload{strings.NewReader(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.Body.String(); got != payload {
		t.Fatalf("body = %q, want %q", got, payload)
	}
	for header, want := range map[string]string{
		"Content-Length":      "19",
		"Content-Disposition": `attachment; filename="camera file.heic"`,
		"Accept-Ranges":       "bytes",
		"Last-Modified":       testModTime.Format(http.TimeFormat),
	} {
		if got := recorder.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}

// An interrupted download resumes from a byte range instead of from zero.
func TestServeDownloadAnswersRange(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/download?path=file.bin", nil)
	request.Header.Set("Range", "bytes=2-5")
	recorder, err := serveTestDownload(request, testDownload{strings.NewReader("0123456789")})
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "2345" {
		t.Fatalf("got %d %q, want 206 %q", recorder.Code, recorder.Body.String(), "2345")
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", got)
	}
}

// failingDownload is a device file whose reads fail after the headers are sent.
type failingDownload struct{ testDownload }

func (failingDownload) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// A read failure after the headers are sent must surface, not end as a
// silently truncated download.
func TestServeDownloadReportsFailedRead(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/download?path=file.bin", nil)
	_, err := serveTestDownload(request, failingDownload{testDownload{strings.NewReader("0123456789")}})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
}

// blockingDownload is a device file whose first read waits until it is closed.
type blockingDownload struct {
	testDownload
	started   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func (d *blockingDownload) Read([]byte) (int, error) {
	close(d.started)
	<-d.closed
	return 0, io.ErrClosedPipe
}

func (d *blockingDownload) Close() { d.closeOnce.Do(func() { close(d.closed) }) }

func TestServeDownloadDisconnectClosesInFlightRead(t *testing.T) {
	download := &blockingDownload{
		testDownload: testDownload{strings.NewReader("0123456789")},
		started:      make(chan struct{}),
		closed:       make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/download?path=file.bin", nil)
	done := make(chan struct{})
	go func() {
		_, _ = serveTestDownload(request, download)
		close(done)
	}()
	select {
	case <-download.started:
	case <-time.After(time.Second):
		t.Fatal("download did not start reading")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disconnected download stayed blocked")
	}
}
