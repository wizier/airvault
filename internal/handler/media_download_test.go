package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

// testDownload announces size bytes, writes body and then returns err.
type testDownload struct {
	body string
	size int64
	err  error
}

func (d testDownload) Size() int64 { return d.size }
func (d testDownload) Close()      {}
func (d testDownload) CopyTo(_ context.Context, destination io.Writer) error {
	_, _ = io.WriteString(destination, d.body)
	return d.err
}

func TestStreamDeviceDownloadWritesCompleteAttachment(t *testing.T) {
	const payload = "phone file contents"
	recorder := httptest.NewRecorder()
	context := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/download", nil), recorder)

	if err := streamDeviceDownload(context, testDownload{body: payload, size: int64(len(payload))}, "camera file.heic"); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Body.String(); got != payload {
		t.Fatalf("body = %q, want %q", got, payload)
	}
	if got, want := recorder.Header().Get("Content-Length"), "19"; got != want {
		t.Fatalf("Content-Length = %q, want %q", got, want)
	}
	if got := recorder.Header().Get("Content-Disposition"); got != `attachment; filename="camera file.heic"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
}

// A source that ends short must surface as an error, not a finished download.
func TestStreamDeviceDownloadReportsTruncatedSource(t *testing.T) {
	context := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/download", nil), httptest.NewRecorder())
	download := testDownload{body: "short", size: 10, err: io.ErrUnexpectedEOF}
	if err := streamDeviceDownload(context, download, "file.bin"); err != io.ErrUnexpectedEOF {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
}
