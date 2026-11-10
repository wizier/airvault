package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/wizier/airvault/internal/domain"

	"github.com/labstack/echo/v5"
)

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code domain.Code `json:"code"`
}

func (h *Handler) errorHandler(c *echo.Context, err error) {
	// The SPA deliberately aborts stale refreshes when a newer event arrives.
	// The client is already gone, so this is neither a 500 nor a useful response.
	if c.Request().Context().Err() != nil {
		return
	}
	if resp, _ := echo.UnwrapResponse(c.Response()); resp != nil && resp.Committed {
		// Headers are already on the wire, so an error envelope is impossible.
		// Keep the failure observable instead of silently swallowing a truncated
		// attachment or inline response.
		slog.Warn("request failed after response was committed",
			"error", err, "path", c.Request().URL.Path, "status", resp.Status)
		return
	}
	status, body := mapAndLogAPIError(c, err)
	_ = c.JSON(status, errorResponse{Error: body})
}

// Streams that already sent their headers use this to deliver the code in-band.
func mapAndLogAPIError(c *echo.Context, err error) (int, errorBody) {
	status, body := mapAPIError(err)
	if status >= http.StatusInternalServerError {
		slog.Error("request failed", "error", err, "path", c.Request().URL.Path, "status", status, "code", body.Code)
	}
	return status, body
}

func mapAPIError(err error) (int, errorBody) {
	if validation, ok := errors.AsType[*domain.ValidationError](err); ok {
		return http.StatusUnprocessableEntity, errorBody{Code: validation.Code}
	}
	if action, ok := errors.AsType[*domain.ActionError](err); ok {
		return http.StatusConflict, errorBody{Code: action.Code}
	}
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, errorBody{Code: domain.CodeNotFound}
	case errors.Is(err, domain.ErrDeviceOffline):
		return http.StatusConflict, errorBody{Code: domain.CodeDeviceOffline}
	case errors.Is(err, domain.ErrPairingRequired):
		return http.StatusConflict, errorBody{Code: domain.CodePairingRequired}
	case errors.Is(err, domain.ErrPairingCleanup):
		return http.StatusInternalServerError, errorBody{Code: domain.CodePairingCleanupFailed}
	case errors.Is(err, domain.ErrBusy):
		return http.StatusConflict, errorBody{Code: domain.CodeResourceBusy}
	case errors.Is(err, domain.ErrCancelled):
		return http.StatusConflict, errorBody{Code: domain.CodeOperationCancelled}
	case errors.Is(err, domain.ErrOperationState):
		return http.StatusConflict, errorBody{Code: domain.CodeOperationStateConflict}
	}
	// echo.HTTPError and any other HTTPStatusCoder land here.
	if status := echo.StatusCode(err); status >= 400 {
		return status, errorBody{Code: statusCodeName(status)}
	}
	return http.StatusInternalServerError, errorBody{Code: domain.CodeInternalError}
}

func statusCodeName(status int) domain.Code {
	switch status {
	case http.StatusBadRequest:
		return domain.CodeBadRequest
	case http.StatusUnauthorized:
		return domain.CodeAuthenticationRequired
	case http.StatusForbidden:
		return domain.CodeForbidden
	case http.StatusNotFound:
		return domain.CodeNotFound
	case http.StatusMethodNotAllowed:
		return domain.CodeMethodNotAllowed
	case http.StatusRequestEntityTooLarge:
		return domain.CodePayloadTooLarge
	case http.StatusUnsupportedMediaType:
		return domain.CodeUnsupportedMediaType
	default:
		if status >= 500 {
			return domain.CodeInternalError
		}
		return domain.Code("http_" + strconv.Itoa(status))
	}
}
