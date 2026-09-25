package handler

import (
	"net/http"
	"strconv"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type galleryResponse struct {
	Assets   []service.GalleryAsset `json:"assets"`
	Total    int                    `json:"total"`
	Revision string                 `json:"revision"`
}

// [GET] /api/devices/:udid/media/gallery?offset=&limit=&revision=
// One page of a stable camera-roll index; later pages carry its revision.
func (h *Handler) galleryList(c *echo.Context) error {
	udid := c.Param("udid")
	offset, limit, err := pagination(c, 120)
	if err != nil {
		return err
	}
	assets, total, revision, err := h.svc.GalleryPage(
		c.Request().Context(),
		udid,
		offset,
		limit,
		c.QueryParam("revision"),
	)
	if err != nil {
		return err
	}
	if assets == nil {
		assets = []service.GalleryAsset{}
	}
	return c.JSON(http.StatusOK, galleryResponse{Assets: assets, Total: total, Revision: revision})
}

type thumbBatchRequest struct {
	Paths []string `json:"paths"`
}

type thumbBatchResponse struct {
	Thumbs map[string][]byte `json:"thumbs"` // dcim path -> base64 JPEG; missing thumbs omitted
}

// [POST] /api/devices/:udid/media/thumbs  body: {paths:[…]}
// A body-carried batch (not a query string) has no URL-length ceiling; the
// batch reads all thumbnails in one AFC session (one handshake per gallery page).
func (h *Handler) mediaThumbs(c *echo.Context) error {
	var req thumbBatchRequest
	if err := echo.BindBody(c, &req); err != nil {
		return err
	}
	thumbs, err := h.svc.ThumbBatch(c.Request().Context(), c.Param("udid"), req.Paths)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, thumbBatchResponse{Thumbs: thumbs})
}

// [GET] /api/devices/:udid/media/stat?path=
// One media file's size and modified time (a single device stat).
func (h *Handler) mediaStat(c *echo.Context) error {
	stat, err := h.svc.MediaStat(c.Request().Context(), c.Param("udid"), c.QueryParam("path"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, stat)
}

func pagination(c *echo.Context, defaultLimit int) (int, int, error) {
	parse := func(name string, fallback int) (int, error) {
		raw := c.QueryParam(name)
		if raw == "" {
			return fallback, nil
		}
		value, err := strconv.Atoi(raw)
		if err != nil {
			return 0, &domain.ValidationError{Code: "invalid_number", Message: name + " must be an integer"}
		}
		return value, nil
	}
	offset, err := parse("offset", 0)
	if err != nil {
		return 0, 0, err
	}
	limit, err := parse("limit", defaultLimit)
	if err != nil {
		return 0, 0, err
	}
	return offset, limit, nil
}
