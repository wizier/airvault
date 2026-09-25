package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

type testDownload struct {
	*bytes.Reader
}

func (d *testDownload) Size() int64 { return int64(d.Reader.Size()) }
func (d *testDownload) Close()      {}
func (d *testDownload) CopyTo(_ context.Context, destination io.Writer) error {
	_, err := io.Copy(destination, d)
	return err
}

func TestStreamDeviceDownloadWritesCompleteAttachment(t *testing.T) {
	payload := []byte("phone file contents")
	download := &testDownload{Reader: bytes.NewReader(payload)}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/download", nil)
	context := echo.New().NewContext(request, recorder)

	if err := streamDeviceDownload(context, download, "camera file.heic"); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Body.Bytes(); !bytes.Equal(got, payload) {
		t.Fatalf("body = %q, want %q", got, payload)
	}
	if got, want := recorder.Header().Get("Content-Length"), "19"; got != want {
		t.Fatalf("Content-Length = %q, want %q", got, want)
	}
	if got := recorder.Header().Get("Content-Disposition"); got != `attachment; filename="camera file.heic"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
}

type failingDownload struct{}

func (d *failingDownload) Size() int64 { return 10 }
func (d *failingDownload) Close()      {}
func (d *failingDownload) CopyTo(_ context.Context, destination io.Writer) error {
	_, _ = destination.Write([]byte("short"))
	return io.ErrUnexpectedEOF
}

func TestStreamDeviceDownloadReportsTruncatedSource(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/download", nil)
	context := echo.New().NewContext(request, recorder)

	err := streamDeviceDownload(context, &failingDownload{}, "file.bin")
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
}
