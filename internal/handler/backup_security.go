package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

type changeBackupPasswordRequest struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// [POST] /api/devices/:udid/backup-password
func (h *Handler) changeBackupPassword(c *echo.Context) error {
	var request changeBackupPasswordRequest
	if err := echo.BindBody(c, &request); err != nil {
		return err
	}
	if err := h.svc.ChangeBackupPassword(
		c.Request().Context(), c.Param("udid"), request.Old, request.New,
	); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
