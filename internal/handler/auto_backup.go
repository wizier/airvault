package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/wizier/airvault/internal/service"
)

func (h *Handler) setAutoBackup(c *echo.Context) error {
	var settings service.AutoBackupSettings
	if err := echo.BindBody(c, &settings); err != nil {
		return err
	}
	if err := h.svc.SetAutoBackup(c.Request().Context(), c.Param("udid"), settings); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
