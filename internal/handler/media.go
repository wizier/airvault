package handler

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/domain"

	"github.com/labstack/echo/v5"
)

type deviceFilesResponse struct {
	Entries []devicefs.Entry `json:"entries"`
}

func writeDeviceFiles(c *echo.Context, entries []devicefs.Entry) error {
	if entries == nil {
		entries = []devicefs.Entry{} // serialize an empty directory as []
	}
	return c.JSON(http.StatusOK, deviceFilesResponse{Entries: entries})
}

// [GET] /api/devices/:udid/media?path=
func (h *Handler) listMedia(c *echo.Context) error {
	entries, err := h.svc.MediaList(c.Request().Context(), c.Param("udid"), c.QueryParam("path"))
	if err != nil {
		return err
	}
	return writeDeviceFiles(c, entries)
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
	if name == "" || name == "/" || name == "." {
		return "file"
	}
	return name
}

// [GET] /api/devices/:udid/media/download?path=
// Streams one file from the device media partition to the browser as an attachment.
func (h *Handler) downloadMedia(c *echo.Context) error {
	devPath, err := requiredPathParam(c)
	if err != nil {
		return err
	}
	downloadID, err := optionalDownloadID(c)
	if err != nil {
		return err
	}
	download, err := h.svc.OpenMediaDownload(c.Request().Context(), c.Param("udid"), devPath, downloadID)
	if err != nil {
		return err
	}
	defer download.Close()
	return streamDeviceDownload(c, download, downloadFilename(devPath))
}

type deviceDownload interface {
	Size() int64
	CopyTo(context.Context, io.Writer) error
}

// closableDownload also releases its phone transport; the image preview closes it
// early, before HEIC decode and the browser send.
type closableDownload interface {
	deviceDownload
	Close()
}

func streamDeviceDownload(c *echo.Context, download deviceDownload, filename string) error {
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = "attachment"
	}
	return streamDeviceFile(c, download, "application/octet-stream", disposition)
}

func streamDeviceFile(c *echo.Context, download deviceDownload, contentType, disposition string) error {
	response := c.Response()
	response.Header().Set(echo.HeaderContentType, contentType)
	response.Header().Set(echo.HeaderContentDisposition, disposition)
	response.Header().Set("Content-Length", strconv.FormatInt(download.Size(), 10))
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.WriteHeader(http.StatusOK)
	err := download.CopyTo(c.Request().Context(), response)
	if c.Request().Context().Err() != nil {
		return nil
	}
	return err
}

func optionalDownloadID(c *echo.Context) (string, error) {
	value := c.QueryParam("downloadId")
	if value == "" {
		return "", nil
	}
	// Ephemeral SSE correlation key, not a domain id; just bound its length.
	if len(value) > 128 {
		return "", &domain.ValidationError{Code: "download_id_too_long", Message: "downloadId is too long"}
	}
	return value, nil
}

// [GET] /api/devices/:udid/media/preview?path=
// Renders a native image or a pure-Go HEIC conversion inline.
func (h *Handler) previewMedia(c *echo.Context) error {
	devPath, err := requiredPathParam(c)
	if err != nil {
		return err
	}
	download, err := h.svc.OpenMediaDownload(c.Request().Context(), c.Param("udid"), devPath, "")
	if err != nil {
		return err
	}
	defer download.Close()
	return streamImagePreview(c, download, path.Base(devPath))
}

// streamImagePreview renders an already-open device download inline: a browser-
// native image as-is, or a pure-Go HEIC→JPEG conversion. Shared by the media and
// app-Documents file browsers so preview behaves identically in both.
func streamImagePreview(c *echo.Context, download closableDownload, name string) error {
	kind := classifyPreview(name)
	if kind == previewNone {
		return newPublicError(http.StatusUnsupportedMediaType, "unsupported_media_type", nil)
	}
	const maxPreviewBytes = 256 << 20
	if download.Size() > maxPreviewBytes {
		return newPublicError(http.StatusRequestEntityTooLarge, "payload_too_large", nil)
	}
	if kind == previewImageNative {
		c.Response().Header().Set("Cache-Control", "private, max-age=300")
		return streamDeviceFile(c, download, nativeImageContentType(name), "inline")
	}
	// HEIC needs a seekable input for decoding. Removing or replacing the <img>
	// cancels this request and closes the device stream while the copy is active.
	orig, err := os.CreateTemp("", "airvault-preview-*")
	if err != nil {
		return fmt.Errorf("create temporary preview: %w", err)
	}
	defer orig.Close()
	defer os.Remove(orig.Name())
	if err := download.CopyTo(c.Request().Context(), orig); err != nil {
		return fmt.Errorf("copy preview: %w", err)
	}
	// No AFC resource is needed past this point.
	download.Close()
	if err := orig.Close(); err != nil {
		return fmt.Errorf("close temporary preview: %w", err)
	}
	jpg, err := renderHEICPreview(orig.Name())
	if err != nil {
		return fmt.Errorf("render preview: %w", err)
	}
	c.Response().Header().Set(echo.HeaderContentDisposition, "inline")
	c.Response().Header().Set("Cache-Control", "private, max-age=300")
	return c.Blob(http.StatusOK, "image/jpeg", jpg)
}
