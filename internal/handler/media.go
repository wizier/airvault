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

type fileListResponse struct {
	Entries []service.FileEntry `json:"entries"`
}

// GET /api/devices/:udid/media?path=
func (h *Handler) listMedia(c *echo.Context) error {
	entries, err := h.svc.MediaList(c.Request().Context(), c.Param("udid"), c.QueryParam("path"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, fileListResponse{Entries: entries})
}

// GET /api/devices/:udid/media/{download,preview}?path=
func (h *Handler) openMedia(c *echo.Context, filePath string) (deviceDownload, error) {
	return h.svc.OpenMediaDownload(c.Request().Context(), c.Param("udid"), filePath)
}

type deviceDownload interface {
	io.ReadSeeker
	Size() int64
	ModTime() time.Time
	Close()
}

// serveFile answers with the file open finds at the request's path, as serve
// sends it: a download, a preview or its facts.
func serveFile(open func(*echo.Context, string) (deviceDownload, error),
	serve func(*echo.Context, deviceDownload, string) error) echo.HandlerFunc {
	return func(c *echo.Context) error {
		filePath := c.QueryParam("path")
		download, err := open(c, filePath)
		if err != nil {
			return err
		}
		return serve(c, download, path.Base(filePath))
	}
}

func serveStat(c *echo.Context, download deviceDownload, _ string) error {
	defer download.Close()
	stat := service.DeviceFileStat{Size: download.Size()}
	if modified := download.ModTime(); !modified.IsZero() {
		stat.Modified = new(modified.Unix())
	}
	return c.JSON(http.StatusOK, stat)
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

// Video and audio stream as they are, Range and all, so the browser can seek.
func streamPreview(c *echo.Context, download deviceDownload, name string) error {
	context.AfterFunc(c.Request().Context(), download.Close)
	defer download.Close()
	ext := strings.ToLower(path.Ext(name))
	if contentType, stream := streamType[ext]; stream {
		return serveDeviceFile(c, download, name, contentType, "inline")
	}
	contentType, native := nativeImageType[ext]
	if !native && !transcodeImageExt[ext] {
		return echo.ErrUnsupportedMediaType
	}
	// Safari shows HEIC itself and says so in Accept; the transcode is for the rest.
	if transcodeImageExt[ext] {
		c.Response().Header().Add("Vary", "Accept")
		if strings.Contains(c.Request().Header.Get("Accept"), "image/heic") {
			contentType, native = "image/heic", true
		}
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
