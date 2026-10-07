package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type powerRequest struct {
	Action service.PowerAction `json:"action"`
}

// POST /api/devices/:udid/power
func (h *Handler) controlPower(c *echo.Context) error {
	var request powerRequest
	if err := echo.BindBody(c, &request); err != nil {
		return err
	}
	if err := h.svc.Power(c.Request().Context(), c.Param("udid"), request.Action); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// POST /api/devices/:udid/erase
func (h *Handler) eraseDevice(c *echo.Context) error {
	if err := h.svc.EraseDevice(c.Request().Context(), c.Param("udid")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
