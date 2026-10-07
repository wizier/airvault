package handler

import (
	"context"
	"net/http"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type restorePointsResponse struct {
	RestorePoints []service.RestorePoint `json:"restorePoints"`
}

// POST /api/devices/:udid/backup
func (h *Handler) startBackup(c *echo.Context) error {
	runID, err := h.svc.StartBackup(c.Request().Context(), c.Param("udid"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, acceptedRunResponse{RunID: runID})
}

// POST /api/devices/:udid/verify
func (h *Handler) startVerify(c *echo.Context) error {
	runID, err := h.svc.StartVerify(c.Request().Context(), c.Param("udid"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, acceptedRunResponse{RunID: runID})
}

// GET /api/devices/:udid/backups
func (h *Handler) listBackups(c *echo.Context) error {
	points, err := h.svc.RestorePoints(c.Request().Context(), c.Param("udid"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, restorePointsResponse{RestorePoints: points})
}

// Wiping the whole source requires an explicit all=true. An empty selector is
// an error, so a dropped query can never widen a deletion.
// DELETE /api/devices/:udid/backups?id=&all=
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

// HEAD lets the UI check the download first; Range resumes a cut one.
// {GET,HEAD} /api/backups/:snapshotId/download
func (h *Handler) downloadBackup(c *echo.Context) error {
	export, err := h.svc.OpenBackupExport(c.Request().Context(), c.Param("snapshotId"))
	if err != nil {
		return err
	}
	return serveDownload(c, export, export.Name)
}

// GET /api/devices/:udid/backups/reclaimable?id=
func (h *Handler) snapshotsReclaimable(c *echo.Context) error {
	bytes, err := h.svc.SnapshotsReclaimable(c.Request().Context(), c.Param("udid"), c.QueryParams()["id"])
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]int64{"reclaimableBytes": bytes})
}

type unlockBackupRequest struct {
	Password string `json:"password"`
}

// POST /api/backups/:snapshotId/unlock
func (h *Handler) unlockBackup(c *echo.Context) error {
	var request unlockBackupRequest
	if err := echo.BindBody(c, &request); err != nil {
		return err
	}
	if err := h.svc.UnlockBackup(c.Request().Context(), c.Param("snapshotId"), request.Password); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// backupJSON answers with what read finds in the restore point, as {"<key>": …}.
func backupJSON[T any](key string, read func(ctx context.Context, snapshotID string) (T, error)) echo.HandlerFunc {
	return func(c *echo.Context) error {
		value, err := read(c.Request().Context(), c.Param("snapshotId"))
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, map[string]T{key: value})
	}
}

// GET /api/backups/:snapshotId/photos/gallery?filter=&month=&offset=&limit=
func (h *Handler) backupPhotos(c *echo.Context) error {
	offset, limit, err := pageQuery(c)
	if err != nil {
		return err
	}
	assets, total, err := h.svc.BackupPhotos(c.Request().Context(), c.Param("snapshotId"),
		c.QueryParam("filter"), c.QueryParam("month"), offset, limit)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, galleryResponse{Assets: assets, Total: total})
}

// GET /api/backups/:snapshotId/photos/months?filter=
func (h *Handler) backupPhotoMonths(c *echo.Context) error {
	months, err := h.svc.BackupPhotoMonths(c.Request().Context(), c.Param("snapshotId"), c.QueryParam("filter"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string][]service.PhotoMonth{"months": months})
}

// POST /api/backups/:snapshotId/photos/thumbs
func (h *Handler) backupPhotoThumbs(c *echo.Context) error {
	var req thumbBatchRequest
	if err := echo.BindBody(c, &req); err != nil {
		return err
	}
	thumbs, err := h.svc.BackupPhotoThumbs(c.Request().Context(), c.Param("snapshotId"), req.Paths)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, thumbBatchResponse{Thumbs: thumbs})
}

// component is the part of the backup a route names; one it does not know
// finds nothing.
func component(c *echo.Context) iosbackup.Component { return iosbackup.Component(c.Param("component")) }

// GET /api/backups/:snapshotId/:component/chats
func (h *Handler) backupChats(c *echo.Context) error {
	chats, err := h.svc.BackupChats(c.Request().Context(), c.Param("snapshotId"), component(c))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string][]iosbackup.Chat{"chats": chats})
}

// GET /api/backups/:snapshotId/:component/chats/messages?chat=&offset=&limit=
func (h *Handler) backupMessages(c *echo.Context) error {
	chatIDs, err := echo.QueryParamsOr[int64](c, "chat", nil)
	if err != nil {
		return err
	}
	offset, limit, err := pageQuery(c)
	if err != nil {
		return err
	}
	messages, err := h.svc.BackupMessages(c.Request().Context(), c.Param("snapshotId"), component(c), chatIDs, offset, limit)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string][]iosbackup.Message{"messages": messages})
}

// GET /api/backups/:snapshotId/:component/chats/matches?chat=&q=
func (h *Handler) backupChatSearch(c *echo.Context) error {
	chatIDs, err := echo.QueryParamsOr[int64](c, "chat", nil)
	if err != nil {
		return err
	}
	matches, err := h.svc.BackupChatSearch(c.Request().Context(), c.Param("snapshotId"), component(c), chatIDs, c.QueryParam("q"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string][]iosbackup.Match{"matches": matches})
}

// GET /api/backups/:snapshotId/:component/matches?q=
func (h *Handler) backupSearch(c *echo.Context) error {
	found, err := h.svc.BackupSearch(c.Request().Context(), c.Param("snapshotId"), component(c), c.QueryParam("q"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string][]iosbackup.Found{"matches": found})
}

// GET /api/backups/:snapshotId/contacts/:contactId/photo
func (h *Handler) backupContactPhoto(c *echo.Context) error {
	contactID, err := echo.PathParam[int64](c, "contactId")
	if err != nil {
		return err
	}
	photo, err := h.svc.BackupContactPhoto(c.Request().Context(), c.Param("snapshotId"), contactID)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", "private, max-age=300")
	return c.Blob(http.StatusOK, http.DetectContentType(photo), photo)
}

// GET /api/backups/:snapshotId/apps/:bundleId/icon
func (h *Handler) backupAppIcon(c *echo.Context) error {
	icon, err := h.svc.BackupAppIcon(c.Request().Context(), c.Param("snapshotId"), c.Param("bundleId"))
	if err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", "private, max-age=300")
	return c.Blob(http.StatusOK, "image/png", icon)
}

// GET /api/backups/:snapshotId/:component/files?path=
func (h *Handler) backupFolder(c *echo.Context) error {
	entries, err := h.svc.BackupFolder(c.Request().Context(), c.Param("snapshotId"), component(c), c.QueryParam("path"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, fileListResponse{Entries: entries})
}

// openBackupFile opens a file of the component a request names.
// GET /api/backups/:snapshotId/:component/files/{stat,download,preview}?path=
func (h *Handler) openBackupFile(c *echo.Context, filePath string) (deviceDownload, error) {
	return h.svc.OpenBackupFile(c.Request().Context(), c.Param("snapshotId"), component(c), filePath)
}
