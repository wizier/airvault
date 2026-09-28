package handler

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type deviceFilesResponse struct {
	Entries []service.FileEntry `json:"entries"`
}

func (h *Handler) listMedia(c *echo.Context) error {
	entries, err := h.svc.MediaList(c.Request().Context(), c.Param("udid"), c.QueryParam("path"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, deviceFilesResponse{Entries: entries})
}

func (h *Handler) downloadMedia(c *echo.Context) error {
	devPath := c.QueryParam("path")
	download, err := h.svc.OpenMediaDownload(c.Request().Context(), c.Param("udid"), devPath)
	if err != nil {
		return err
	}
	return serveDownload(c, download, path.Base(devPath))
}

func (h *Handler) previewMedia(c *echo.Context) error {
	devPath := c.QueryParam("path")
	download, err := h.svc.OpenMediaDownload(c.Request().Context(), c.Param("udid"), devPath)
	if err != nil {
		return err
	}
	return streamImagePreview(c, download, path.Base(devPath))
}

type deviceDownload interface {
	io.ReadSeeker
	Size() int64
	ModTime() time.Time
	Close()
}

// A client disconnect closes the download at once, which unblocks an in-flight
// phone read and releases the lease.
func serveDownload(c *echo.Context, download deviceDownload, name string) error {
	context.AfterFunc(c.Request().Context(), download.Close)
	defer download.Close()
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	return serveDeviceFile(c, download, name, "application/octet-stream", disposition)
}

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

func streamImagePreview(c *echo.Context, download deviceDownload, name string) error {
	context.AfterFunc(c.Request().Context(), download.Close)
	defer download.Close()
	ext := strings.ToLower(path.Ext(name))
	contentType, native := nativeImageType[ext]
	if !native && !transcodeImageExt[ext] {
		return echo.ErrUnsupportedMediaType
	}
	const maxPreviewBytes = 256 << 20
	if download.Size() > maxPreviewBytes {
		return echo.ErrStatusRequestEntityTooLarge
	}
	// Cache-Control is set per branch, never up front: a failed HEIC decode must
	// not be cached.
	if native {
		c.Response().Header().Set("Cache-Control", "private, max-age=300")
		return serveDeviceFile(c, download, name, contentType, "inline")
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
