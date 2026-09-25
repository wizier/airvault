package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

// restoreRequest mirrors service.RestoreOptions field for field. Seeded with
// the defaults before binding, an omitted field keeps the standard behavior and
// an explicit false stays an override.
type restoreRequest struct {
	SnapshotID             string `json:"snapshotId"`
	Password               string `json:"password"`
	SystemFiles            bool   `json:"systemFiles"`
	Reboot                 bool   `json:"reboot"`
	SettingsFromBackup     bool   `json:"settingsFromBackup"`
	RemoveItemsNotRestored bool   `json:"removeItemsNotRestored"`
}

// [POST] /api/devices/:udid/restore
func (h *Handler) startRestore(c *echo.Context) error {
	request := restoreRequest(service.DefaultRestoreOptions())
	if err := echo.BindBody(c, &request); err != nil {
		return err
	}
	runID, err := h.svc.StartRestore(c.Request().Context(), c.Param("udid"), service.RestoreOptions(request))
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
