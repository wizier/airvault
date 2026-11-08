package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/wizier/airvault/internal/service"
)

// PUT /api/devices/:udid/automatic-tasks
func (h *Handler) setAutomaticTasks(c *echo.Context) error {
	var tasks service.AutomaticTasks
	if err := echo.BindBody(c, &tasks); err != nil {
		return err
	}
	if err := h.svc.SetAutomaticTasks(c.Request().Context(), c.Param("udid"), tasks); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
