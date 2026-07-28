package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

type powerRequest struct {
	Action string `json:"action"`
}

// [POST] /api/devices/:udid/power
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
