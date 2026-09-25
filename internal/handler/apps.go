package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type listAppsResponse struct {
	Apps []service.App `json:"apps"`
}

// [GET] /api/devices/:udid/apps
func (h *Handler) listApps(c *echo.Context) error {
	udid := c.Param("udid")
	apps, err := h.svc.Apps(c.Request().Context(), udid)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, listAppsResponse{Apps: apps})
}

// [GET] /api/devices/:udid/apps/:bundle/icon
func (h *Handler) getAppIcon(c *echo.Context) error {
	udid := c.Param("udid")
	bundle := c.Param("bundle")
	png, err := h.svc.AppIcon(c.Request().Context(), udid, bundle)
	if err != nil {
		return err // 404 / 409 offline
	}
	c.Response().Header().Set("Cache-Control", "public, max-age=86400")
	return c.Blob(http.StatusOK, "image/png", png)
}

// [DELETE] /api/devices/:udid/apps/:bundle
func (h *Handler) uninstallApp(c *echo.Context) error {
	udid := c.Param("udid")
	bundle := c.Param("bundle")
	if err := h.svc.UninstallApp(c.Request().Context(), udid, bundle); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// [GET] /api/devices/:udid/apps/:bundle/files?path=
func (h *Handler) listAppFiles(c *echo.Context) error {
	entries, err := h.svc.AppFiles(
		c.Request().Context(), c.Param("udid"), c.Param("bundle"), c.QueryParam("path"),
	)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, deviceFilesResponse{Entries: entries})
}

// [GET] /api/devices/:udid/apps/:bundle/files/download?path=
// Streams one file from the app's Documents to the browser as an attachment.
func (h *Handler) downloadAppFile(c *echo.Context) error {
	return serveDownload(c, func(devPath string) (*service.DeviceDownload, error) {
		return h.svc.OpenAppFileDownload(c.Request().Context(), c.Param("udid"), c.Param("bundle"), devPath)
	})
}

// [GET] /api/devices/:udid/apps/:bundle/files/preview?path=
// Inline image preview for app Documents — same native/HEIC path as media.
func (h *Handler) previewAppFile(c *echo.Context) error {
	return servePreview(c, func(devPath string) (*service.DeviceDownload, error) {
		return h.svc.OpenAppFileDownload(c.Request().Context(), c.Param("udid"), c.Param("bundle"), devPath)
	})
}

// [DELETE] /api/devices/:udid/apps/:bundle/files?path=
func (h *Handler) deleteAppFile(c *echo.Context) error {
	devPath, err := requiredPathParam(c)
	if err != nil {
		return err
	}
	if err := h.svc.AppFileDelete(c.Request().Context(),
		c.Param("udid"), c.Param("bundle"), devPath); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
