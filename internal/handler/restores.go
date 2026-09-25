package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

// [POST] /api/devices/:udid/restore
// Binding over the defaults keeps them for omitted fields, while an explicit
// false stays an override.
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

type restoreSourcesResponse struct {
	RestoreSources []service.RestorePoint `json:"restoreSources"`
}

// [GET] /api/restore-sources
func (h *Handler) listRestoreSources(c *echo.Context) error {
	sources, err := h.svc.RestoreSources(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, restoreSourcesResponse{RestoreSources: sources})
}
