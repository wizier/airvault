package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type listAppsResponse struct {
	Apps []service.App `json:"apps"`
}

func (h *Handler) listApps(c *echo.Context) error {
	udid := c.Param("udid")
	apps, err := h.svc.Apps(c.Request().Context(), udid)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, listAppsResponse{Apps: apps})
}

type appIconsRequest struct {
	BundleIDs []string `json:"bundleIds"`
}

type appIconsResponse struct {
	Icons map[string][]byte `json:"icons"` // bundle id -> base64 PNG; apps without an icon omitted
}

func (h *Handler) appIcons(c *echo.Context) error {
	var req appIconsRequest
	if err := echo.BindBody(c, &req); err != nil {
		return err
	}
	icons, err := h.svc.AppIcons(c.Request().Context(), c.Param("udid"), req.BundleIDs)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, appIconsResponse{Icons: icons})
}

func (h *Handler) uninstallApp(c *echo.Context) error {
	udid := c.Param("udid")
	bundle := c.Param("bundle")
	if err := h.svc.UninstallApp(c.Request().Context(), udid, bundle); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) listAppFiles(c *echo.Context) error {
	entries, err := h.svc.AppFiles(
		c.Request().Context(), c.Param("udid"), c.Param("bundle"), c.QueryParam("path"),
	)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, deviceFilesResponse{Entries: entries})
}

func (h *Handler) appFileStat(c *echo.Context) error {
	stat, err := h.svc.AppFileStat(c.Request().Context(), c.Param("udid"), c.Param("bundle"), c.QueryParam("path"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, stat)
}

func (h *Handler) openAppFile(c *echo.Context, filePath string) (deviceDownload, error) {
	return h.svc.OpenAppFileDownload(c.Request().Context(), c.Param("udid"), c.Param("bundle"), filePath)
}

func (h *Handler) deleteAppFile(c *echo.Context) error {
	err := h.svc.AppFileDelete(c.Request().Context(), c.Param("udid"), c.Param("bundle"), c.QueryParam("path"))
	if err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
