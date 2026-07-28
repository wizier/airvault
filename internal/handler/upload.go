package handler

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/wizier/airvault/internal/domain"

	"github.com/labstack/echo/v5"
)

// [POST] /api/devices/:udid/apps/install?installId=
func (h *Handler) installApp(c *echo.Context) error {
	udid := c.Param("udid")
	installID := c.QueryParam("installId")
	// Ephemeral SSE correlation key, not a domain identifier.
	if installID == "" || len(installID) > 128 {
		return &domain.ValidationError{Code: "invalid_install_id", Message: "installId is required and must be at most 128 bytes"}
	}
	form, err := c.Request().MultipartReader()
	if err != nil {
		return &domain.ValidationError{Code: "ipa_required", Message: `attach an .ipa file in the "ipa" field`}
	}
	for {
		part, err := form.NextPart()
		if errors.Is(err, io.EOF) {
			return &domain.ValidationError{Code: "ipa_required", Message: `attach an .ipa file in the "ipa" field`}
		}
		if err != nil {
			return err
		}
		if part.FormName() != "ipa" {
			if err := part.Close(); err != nil {
				return err
			}
			continue
		}
		defer part.Close()
		if !strings.HasSuffix(strings.ToLower(part.FileName()), ".ipa") {
			return &domain.ValidationError{Code: "invalid_ipa", Message: "the file must be an .ipa"}
		}
		if err := h.svc.InstallApp(c.Request().Context(), udid, installID, part); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
