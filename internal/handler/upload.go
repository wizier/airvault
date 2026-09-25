package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"

	"github.com/labstack/echo/v5"
)

// [POST] /api/devices/:udid/apps/install
func (h *Handler) installApp(c *echo.Context) error {
	udid := c.Param("udid")
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
		return streamInstall(c, func(onProgress func(engine.InstallProgress)) error {
			return h.svc.InstallApp(c.Request().Context(), udid, part, onProgress)
		})
	}
}

type installProgressLine struct {
	Phase   engine.InstallPhase `json:"phase"`
	Percent int                 `json:"percent"`
}

type installResultLine struct {
	Done  bool       `json:"done,omitempty"`
	Error *errorBody `json:"error,omitempty"`
}

// streamInstall runs install and answers with NDJSON once it reports progress:
// one {"phase","percent"} line per update, then {"done":true} or
// {"error":{"code"}}. A failure before any progress stays a plain JSON error.
func streamInstall(c *echo.Context, install func(onProgress func(engine.InstallProgress)) error) error {
	var controller *http.ResponseController
	var writeErr error
	writeLine := func(line any) {
		if controller == nil {
			controller = startStream(c, "application/x-ndjson")
		}
		if writeErr != nil {
			return // the client stopped reading; the install still runs to its end
		}
		data, _ := json.Marshal(line) // fixed shapes of strings, ints and bools
		writeErr = writeStreamFrame(controller, c.Response(), string(data)+"\n")
	}
	err := install(func(progress engine.InstallProgress) {
		writeLine(installProgressLine{Phase: progress.Phase, Percent: progress.Percent})
	})
	switch {
	case err != nil && controller == nil:
		return err
	case err != nil:
		status, body := mapAPIError(err)
		if status >= http.StatusInternalServerError {
			slog.Error("install failed after streaming began", "error", err, "code", body.Code)
		}
		writeLine(installResultLine{Error: &body})
	default:
		writeLine(installResultLine{Done: true})
	}
	return nil
}
