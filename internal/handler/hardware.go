package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// GET /api/devices/:udid/hardware
func (h *Handler) getHardware(c *echo.Context) error {
	hardware, err := h.svc.HardwareInfo(c.Request().Context(), c.Param("udid"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, hardware)
}
