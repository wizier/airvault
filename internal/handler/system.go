package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type statusResponse struct {
	Muxer   service.MuxerStatus   `json:"muxer"`
	Running []service.RunProgress `json:"running"`
}

// GET /api/status
func (h *Handler) status(c *echo.Context) error {
	return c.JSON(http.StatusOK, statusResponse{Muxer: h.svc.Muxer(), Running: h.svc.Running()})
}
