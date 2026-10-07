package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/wizier/airvault/internal/service"
)

// PUT /api/devices/:udid/cleanup
func (h *Handler) setCleanup(c *echo.Context) error {
	var settings service.CleanupSettings
	if err := echo.BindBody(c, &settings); err != nil {
		return err
	}
	if err := h.svc.SetCleanup(c.Request().Context(), c.Param("udid"), settings); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// GET /api/devices/:udid/cleanup/plan?keepDays=&thin=
func (h *Handler) planCleanup(c *echo.Context) error {
	keepDays, err := echo.QueryParamOr(c, "keepDays", 0)
	if err != nil {
		return err
	}
	settings := service.CleanupSettings{KeepDays: keepDays, Thin: service.ThinPeriod(c.QueryParam("thin"))}
	remove, err := h.svc.PlanCleanup(c.Request().Context(), c.Param("udid"), settings)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string][]string{"remove": remove})
}
