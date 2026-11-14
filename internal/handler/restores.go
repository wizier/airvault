package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

// Binding over the defaults keeps them for omitted fields, while an explicit
// false stays an override.
// POST /api/devices/:udid/restore
func (h *Handler) startRestore(c *echo.Context) error {
	opts := service.DefaultRestoreOptions()
	if err := echo.BindBody(c, &opts); err != nil {
		return err
	}
	runID, err := h.svc.StartRestore(c.Request().Context(), c.Param("udid"), opts)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, acceptedRunResponse{RunID: runID})
}

type restorePointsResponse struct {
	RestorePoints []service.RestorePoint `json:"restorePoints"`
}

// GET /api/restore-points
func (h *Handler) listRestorePoints(c *echo.Context) error {
	points, err := h.svc.RestorePoints(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, restorePointsResponse{RestorePoints: points})
}
