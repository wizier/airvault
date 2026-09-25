package handler

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type deviceFilesResponse struct {
	Entries []service.FileEntry `json:"entries"`
}

// [GET] /api/devices/:udid/media?path=
func (h *Handler) listMedia(c *echo.Context) error {
	entries, err := h.svc.MediaList(c.Request().Context(), c.Param("udid"), c.QueryParam("path"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, deviceFilesResponse{Entries: entries})
}

// requiredPathParam reads the mandatory ?path= query parameter.
func requiredPathParam(c *echo.Context) (string, error) {
	devPath := c.QueryParam("path")
	if devPath == "" {
		return "", &domain.ValidationError{Code: "path_required", Message: "path is required"}
	}
	return devPath, nil
}

// [GET] /api/devices/:udid/media/download?path=
// Serves one file from the device media partition to the browser as an attachment.
func (h *Handler) downloadMedia(c *echo.Context) error {
	return serveDownload(c, func(devPath string) (deviceDownload, error) {
		return h.svc.OpenMediaDownload(c.Request().Context(), c.Param("udid"), devPath)
	})
}

// [GET] /api/devices/:udid/media/preview?path=
// Renders a native image or a pure-Go HEIC conversion inline.
func (h *Handler) previewMedia(c *echo.Context) error {
	return servePreview(c, func(devPath string) (deviceDownload, error) {
		return h.svc.OpenMediaDownload(c.Request().Context(), c.Param("udid"), devPath)
	})
}

// openDownload opens the device file named by the request's ?path=.
type openDownload func(devPath string) (deviceDownload, error)

// deviceDownload is one open device file; Close releases its phone transport.
type deviceDownload interface {
	io.ReadSeeker
	Size() int64
	ModTime() time.Time
	Close()
}

func serveDownload(c *echo.Context, open openDownload) error {
	download, name, err := openRequestedFile(c, open)
	if err != nil {
		return err
	}
	defer download.Close()
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	return serveDeviceFile(c, download, name, "application/octet-stream", disposition)
}

func servePreview(c *echo.Context, open openDownload) error {
	download, name, err := openRequestedFile(c, open)
	if err != nil {
		return err
	}
	defer download.Close()
	return streamImagePreview(c, download, name)
}

// openRequestedFile opens the file named by ?path=. A client disconnect closes
// it, which unblocks an in-flight phone read and releases the lease at once.
func openRequestedFile(c *echo.Context, open openDownload) (deviceDownload, string, error) {
	devPath, err := requiredPathParam(c)
	if err != nil {
		return nil, "", err
	}
	download, err := open(devPath)
	if err != nil {
		return nil, "", err
	}
	context.AfterFunc(c.Request().Context(), download.Close)
	return download, path.Base(devPath), nil
}

// serveDeviceFile answers a whole-file or Range request, with Last-Modified.
func serveDeviceFile(c *echo.Context, download deviceDownload, name, contentType, disposition string) error {
	header := c.Response().Header()
	header.Set(echo.HeaderContentType, contentType)
	header.Set(echo.HeaderContentDisposition, disposition)
	content := &readFailure{deviceDownload: download}
	http.ServeContent(c.Response(), c.Request(), name, download.ModTime(), content)
	return content.err
}

// readFailure keeps a device read error that http.ServeContent drops once the
// headers are sent, so a truncated response is still logged.
type readFailure struct {
	deviceDownload
	err error
}

func (r *readFailure) Read(buffer []byte) (int, error) {
	read, err := r.deviceDownload.Read(buffer)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return read, err
}

// streamImagePreview renders an already-open device download inline: a browser-
// native image as-is, or a pure-Go HEIC→JPEG conversion. Shared by the media and
// app-Documents file browsers so preview behaves identically in both.
func streamImagePreview(c *echo.Context, download deviceDownload, name string) error {
	kind := classifyPreview(name)
	if kind == previewNone {
		return &publicError{http.StatusUnsupportedMediaType, "unsupported_media_type"}
	}
	const maxPreviewBytes = 256 << 20
	if download.Size() > maxPreviewBytes {
		return &publicError{http.StatusRequestEntityTooLarge, "payload_too_large"}
	}
	if kind == previewImageNative {
		c.Response().Header().Set("Cache-Control", "private, max-age=300")
		return serveDeviceFile(c, download, name, nativeImageContentType(name), "inline")
	}
	// The decoder buffers its whole input itself, so it reads the device stream
	// directly. Removing or replacing the <img> cancels this request and closes
	// the device stream while the copy is active.
	source, sink := io.Pipe()
	defer source.Close()
	go func() {
		_, err := io.Copy(sink, download)
		download.Close() // release the phone before the CPU-heavy decode
		sink.CloseWithError(err)
	}()
	jpg, err := renderHEICPreview(source)
	if err != nil {
		return fmt.Errorf("render preview: %w", err)
	}
	c.Response().Header().Set(echo.HeaderContentDisposition, "inline")
	c.Response().Header().Set("Cache-Control", "private, max-age=300")
	return c.Blob(http.StatusOK, "image/jpeg", jpg)
}
