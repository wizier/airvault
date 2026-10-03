package handler

import (
	"context"
	"net/http"
	"path"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type restorePointsResponse struct {
	RestorePoints []service.RestorePoint `json:"restorePoints"`
}

func (h *Handler) startBackup(c *echo.Context) error {
	runID, err := h.svc.StartBackup(c.Request().Context(), c.Param("udid"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, acceptedRunResponse{RunID: runID})
}

func (h *Handler) startVerify(c *echo.Context) error {
	runID, err := h.svc.StartVerify(c.Request().Context(), c.Param("udid"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, acceptedRunResponse{RunID: runID})
}

func (h *Handler) listBackups(c *echo.Context) error {
	points, err := h.svc.RestorePoints(c.Request().Context(), c.Param("udid"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, restorePointsResponse{RestorePoints: points})
}

// Wiping the whole source requires an explicit all=true. An empty selector is
// an error, so a dropped query can never widen a deletion.
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
func (h *Handler) downloadBackup(c *echo.Context) error {
	export, err := h.svc.OpenBackupExport(c.Request().Context(), c.Param("snapshotId"))
	if err != nil {
		return err
	}
	return serveDownload(c, export, export.Name)
}

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

type backupComponentsResponse struct {
	Components []iosbackup.Component `json:"components"`
}

func (h *Handler) backupComponents(c *echo.Context) error {
	held, err := h.svc.BackupComponents(c.Request().Context(), c.Param("snapshotId"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, backupComponentsResponse{Components: held})
}

type backupCallsResponse struct {
	Calls []iosbackup.Call `json:"calls"`
}

func (h *Handler) backupCalls(c *echo.Context) error {
	calls, err := h.svc.BackupCalls(c.Request().Context(), c.Param("snapshotId"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, backupCallsResponse{Calls: calls})
}

type backupContactsResponse struct {
	Contacts []iosbackup.Contact `json:"contacts"`
}

func (h *Handler) backupContacts(c *echo.Context) error {
	contacts, err := h.svc.BackupContacts(c.Request().Context(), c.Param("snapshotId"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, backupContactsResponse{Contacts: contacts})
}

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

type photoFiltersResponse struct {
	Filters []service.PhotoFilter `json:"filters"`
}

func (h *Handler) backupPhotoFilters(c *echo.Context) error {
	filters, err := h.svc.BackupPhotoFilters(c.Request().Context(), c.Param("snapshotId"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, photoFiltersResponse{Filters: filters})
}

type photoMonthsResponse struct {
	Months []service.PhotoMonth `json:"months"`
}

func (h *Handler) backupPhotoMonths(c *echo.Context) error {
	months, err := h.svc.BackupPhotoMonths(c.Request().Context(), c.Param("snapshotId"), c.QueryParam("filter"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, photoMonthsResponse{Months: months})
}

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

type backupChatsResponse struct {
	Chats []iosbackup.Chat `json:"chats"`
}

func (h *Handler) backupChats(c *echo.Context) error {
	chats, err := h.svc.BackupChats(c.Request().Context(), c.Param("snapshotId"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, backupChatsResponse{Chats: chats})
}

type backupMessagesResponse struct {
	Messages []iosbackup.Message `json:"messages"`
}

func (h *Handler) backupMessages(c *echo.Context) error {
	chatIDs, err := echo.QueryParams[int64](c, "chat")
	if err != nil {
		return err
	}
	offset, limit, err := pageQuery(c)
	if err != nil {
		return err
	}
	messages, err := h.svc.BackupMessages(c.Request().Context(), c.Param("snapshotId"), chatIDs, offset, limit)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, backupMessagesResponse{Messages: messages})
}

// backupFileOpener opens a file a backup view serves, by its path.
type backupFileOpener func(ctx context.Context, snapshotID, path string) (*service.BackupFileDownload, error)

func backupFileStat(open backupFileOpener) echo.HandlerFunc {
	return func(c *echo.Context) error {
		download, err := open(c.Request().Context(), c.Param("snapshotId"), c.QueryParam("path"))
		if err != nil {
			return err
		}
		defer download.Close()
		stat := service.DeviceFileStat{Size: download.Size()}
		if modified := download.ModTime(); !modified.IsZero() {
			stat.Modified = new(modified.Unix())
		}
		return c.JSON(http.StatusOK, stat)
	}
}

func downloadBackupFile(open backupFileOpener) echo.HandlerFunc {
	return func(c *echo.Context) error {
		filePath := c.QueryParam("path")
		download, err := open(c.Request().Context(), c.Param("snapshotId"), filePath)
		if err != nil {
			return err
		}
		return serveDownload(c, download, path.Base(filePath))
	}
}

func previewBackupFile(open backupFileOpener) echo.HandlerFunc {
	return func(c *echo.Context) error {
		filePath := c.QueryParam("path")
		download, err := open(c.Request().Context(), c.Param("snapshotId"), filePath)
		if err != nil {
			return err
		}
		return streamPreview(c, download, path.Base(filePath))
	}
}
