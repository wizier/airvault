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

// publicError is a handler-authored failure with a stable client-facing code;
// the frontend owns presentation text.
type publicError struct {
	status int
	code   string
}

func (e *publicError) Error() string { return e.code }

// errorHandler renders all HTTP/API failures through the same envelope. Echo
// framework failures (routing, method, body size) are mapped as well as domain
// errors returned by services.
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
	status, body := mapAPIError(err)
	if status >= http.StatusInternalServerError {
		slog.Error("request failed", "error", err, "path", c.Request().URL.Path, "status", status, "code", body.Code)
	}
	_ = c.JSON(status, errorResponse{Error: body})
}

func mapAPIError(err error) (int, errorBody) {
	var public *publicError
	var validation *domain.ValidationError
	var action *domain.ActionError
	switch {
	case errors.As(err, &public):
		return public.status, errorBody{Code: public.code}
	case errors.As(err, &validation):
		return http.StatusUnprocessableEntity, errorBody{Code: validation.Code}
	case errors.As(err, &action):
		return http.StatusConflict, errorBody{Code: action.Code}
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

// statusCodeName names the statuses only the framework produces: bind failures
// (400), CSRF (403), the router (404/405) and the body limit (413). Handler-
// authored errors carry their own code as domain.* or publicError instead.
func statusCodeName(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusRequestEntityTooLarge:
		return "payload_too_large"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "http_" + strconv.Itoa(status)
	}
}
