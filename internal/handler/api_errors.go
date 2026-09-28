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
	Code string `json:"code"`
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
		return http.StatusNotFound, errorBody{Code: "not_found"}
	case errors.Is(err, domain.ErrDeviceOffline):
		return http.StatusConflict, errorBody{Code: "device_offline"}
	case errors.Is(err, domain.ErrPairingRequired):
		return http.StatusConflict, errorBody{Code: "pairing_required"}
	case errors.Is(err, domain.ErrPairingCleanup):
		return http.StatusInternalServerError, errorBody{Code: "pairing_cleanup_failed"}
	case errors.Is(err, domain.ErrBusy):
		return http.StatusConflict, errorBody{Code: "resource_busy"}
	case errors.Is(err, domain.ErrCancelled):
		return http.StatusConflict, errorBody{Code: "operation_cancelled"}
	case errors.Is(err, domain.ErrOperationState):
		return http.StatusConflict, errorBody{Code: "operation_state_conflict"}
	}
	// echo.HTTPError and any other HTTPStatusCoder land here.
	if status := echo.StatusCode(err); status >= 400 {
		return status, errorBody{Code: statusCodeName(status)}
	}
	return http.StatusInternalServerError, errorBody{Code: "internal_error"}
}

func statusCodeName(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "authentication_required"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusRequestEntityTooLarge:
		return "payload_too_large"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "http_" + strconv.Itoa(status)
	}
}
