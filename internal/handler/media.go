package handler

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"

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

// downloadFilename derives an attachment filename from a device path.
func downloadFilename(devPath string) string {
	name := path.Base(devPath)
	if name == "/" || name == "." {
		return "file"
	}
	return name
}

// [GET] /api/devices/:udid/media/download?path=
// Streams one file from the device media partition to the browser as an attachment.
func (h *Handler) downloadMedia(c *echo.Context) error {
	return serveDownload(c, func(devPath string) (*service.DeviceDownload, error) {
		return h.svc.OpenMediaDownload(c.Request().Context(), c.Param("udid"), devPath)
	})
}

// [GET] /api/devices/:udid/media/preview?path=
// Renders a native image or a pure-Go HEIC conversion inline.
func (h *Handler) previewMedia(c *echo.Context) error {
	return servePreview(c, func(devPath string) (*service.DeviceDownload, error) {
		return h.svc.OpenMediaDownload(c.Request().Context(), c.Param("udid"), devPath)
	})
}

// openDownload opens the device file named by the request's ?path=.
type openDownload func(devPath string) (*service.DeviceDownload, error)

// deviceDownload is one open device file; Close releases its phone transport.
type deviceDownload interface {
	Size() int64
	CopyTo(context.Context, io.Writer) error
	Close()
}

func serveDownload(c *echo.Context, open openDownload) error {
	devPath, err := requiredPathParam(c)
	if err != nil {
		return err
	}
	download, err := open(devPath)
	if err != nil {
		return err
	}
	defer download.Close()
	return streamDeviceDownload(c, download, downloadFilename(devPath))
}

func servePreview(c *echo.Context, open openDownload) error {
	devPath, err := requiredPathParam(c)
	if err != nil {
		return err
	}
	download, err := open(devPath)
	if err != nil {
		return err
	}
	defer download.Close()
	return streamImagePreview(c, download, path.Base(devPath))
}

func streamDeviceDownload(c *echo.Context, download deviceDownload, filename string) error {
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	return streamDeviceFile(c, download, "application/octet-stream", disposition)
}

func streamDeviceFile(c *echo.Context, download deviceDownload, contentType, disposition string) error {
	response := c.Response()
	response.Header().Set(echo.HeaderContentType, contentType)
	response.Header().Set(echo.HeaderContentDisposition, disposition)
	response.Header().Set("Content-Length", strconv.FormatInt(download.Size(), 10))
	response.WriteHeader(http.StatusOK)
	err := download.CopyTo(c.Request().Context(), response)
	if c.Request().Context().Err() != nil {
		return nil
	}
	return err
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
		return streamDeviceFile(c, download, nativeImageContentType(name), "inline")
	}
	// The decoder buffers its whole input itself, so it reads the device stream
	// directly. Removing or replacing the <img> cancels this request and closes
	// the device stream while the copy is active.
	source, sink := io.Pipe()
	defer source.Close()
	go func() {
		err := download.CopyTo(c.Request().Context(), sink)
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
