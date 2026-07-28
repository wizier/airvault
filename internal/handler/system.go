package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type statusResponse struct {
	MuxerUp bool                  `json:"muxerUp"`
	Running []service.RunProgress `json:"running"`
}

// [GET] /api/status
func (h *Handler) status(c *echo.Context) error {
	return c.JSON(http.StatusOK, statusResponse{
		MuxerUp: h.svc.MuxerReady(), Running: h.svc.Running(),
	})
}
