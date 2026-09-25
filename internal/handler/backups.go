package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type restorePointsResponse struct {
	RestorePoints []service.RestorePoint `json:"restorePoints"`
}

// [POST] /api/devices/:udid/backup
func (h *Handler) startBackup(c *echo.Context) error {
	runID, err := h.svc.StartBackup(c.Request().Context(), c.Param("udid"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, acceptedRunResponse{RunID: runID})
}

// [GET] /api/devices/:udid/backups
func (h *Handler) listBackups(c *echo.Context) error {
	points, err := h.svc.RestorePoints(c.Request().Context(), c.Param("udid"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, restorePointsResponse{RestorePoints: points})
}

// [DELETE] /api/devices/:udid/backups?id=… — repeatable id selects specific
// restore points; wiping the whole source requires an explicit all=true. An
// empty selector is an error, so a dropped query can never widen a deletion.
func (h *Handler) deleteBackups(c *echo.Context) error {
	ids := c.QueryParams()["id"]
	var err error
	switch {
	case len(ids) > 0:
		err = h.svc.DeleteSnapshots(c.Request().Context(), c.Param("udid"), ids)
	case c.QueryParam("all") == "true":
		err = h.svc.DeleteBackups(c.Request().Context(), c.Param("udid"))
	default:
		err = &domain.ValidationError{Code: "snapshot_required",
			Message: "select restore points (id=…) or pass all=true"}
	}
	if err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// [GET] /api/devices/:udid/backups/reclaimable?id=… (repeatable)
func (h *Handler) snapshotsReclaimable(c *echo.Context) error {
	bytes, err := h.svc.SnapshotsReclaimable(c.Request().Context(), c.Param("udid"), c.QueryParams()["id"])
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]int64{"reclaimableBytes": bytes})
}
