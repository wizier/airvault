package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type listDevicesResponse struct {
	Devices []service.DeviceOverview `json:"devices"`
}

func (h *Handler) listDevices(c *echo.Context) error {
	overviews, err := h.svc.DeviceList(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, listDevicesResponse{Devices: overviews})
}

func (h *Handler) getDeviceBattery(c *echo.Context) error {
	battery, err := h.svc.LiveBattery(c.Request().Context(), c.Param("udid"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, battery)
}

func (h *Handler) getWallpaper(c *echo.Context) error {
	png, err := h.svc.Wallpaper(
		c.Request().Context(), c.Param("udid"), c.QueryParam("screen") == "lock",
	)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.Blob(http.StatusOK, "image/png", png)
}
