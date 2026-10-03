package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type galleryResponse struct {
	Assets   []service.GalleryAsset `json:"assets"`
	Total    int                    `json:"total"`
	Revision string                 `json:"revision"`
}

func pageQuery(c *echo.Context) (offset, limit int, err error) {
	if offset, err = echo.QueryParamOr(c, "offset", 0); err != nil {
		return 0, 0, err
	}
	limit, err = echo.QueryParamOr(c, "limit", 120)
	return offset, limit, err
}

func (h *Handler) galleryList(c *echo.Context) error {
	offset, limit, err := pageQuery(c)
	if err != nil {
		return err
	}
	assets, total, revision, err := h.svc.GalleryPage(
		c.Request().Context(), c.Param("udid"), offset, limit, c.QueryParam("revision"),
	)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, galleryResponse{Assets: assets, Total: total, Revision: revision})
}

type thumbBatchRequest struct {
	Paths []string `json:"paths"`
}

type thumbBatchResponse struct {
	Thumbs map[string][]byte `json:"thumbs"` // dcim path -> base64 JPEG; missing thumbs omitted
}

// A body-carried batch has no URL-length ceiling; it reads all thumbnails in one
// AFC session.
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

func (h *Handler) mediaStat(c *echo.Context) error {
	stat, err := h.svc.MediaStat(c.Request().Context(), c.Param("udid"), c.QueryParam("path"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, stat)
}
