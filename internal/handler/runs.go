package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

type acceptedRunResponse struct {
	RunID string `json:"runId"`
}

func (h *Handler) cancelRun(c *echo.Context) error {
	if err := h.svc.CancelRun(c.Param("id")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
