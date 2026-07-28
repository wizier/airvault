package handler

import (
	"context"
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

// publicError lets a handler attach a stable client-facing code to an otherwise
// untyped error. The frontend owns presentation text; cause is retained only
// for logs and errors.Is/errors.As.
type publicError struct {
	status int
	code   string
	cause  error
}

func (e *publicError) Error() string {
	if e.cause != nil {
		return e.code + ": " + e.cause.Error()
	}
	return e.code
}
func (e *publicError) Unwrap() error { return e.cause }

func newPublicError(status int, code string, cause error) error {
	return &publicError{status: status, code: code, cause: cause}
}

func mapDomainError(err error) (int, errorBody, bool) {
	var validation *domain.ValidationError
	var action *domain.ActionError
	switch {
	case errors.As(err, &validation):
		return http.StatusUnprocessableEntity, errorBody{Code: validation.Code}, true
	case errors.As(err, &action):
		return http.StatusConflict, errorBody{Code: action.Code}, true
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, errorBody{Code: "not_found"}, true
	case errors.Is(err, domain.ErrDeviceOffline):
		return http.StatusConflict, errorBody{Code: "device_offline"}, true
	case errors.Is(err, domain.ErrPairingRequired):
		return http.StatusConflict, errorBody{Code: "pairing_required"}, true
	case errors.Is(err, domain.ErrPairingCleanup):
		return http.StatusInternalServerError, errorBody{Code: "pairing_cleanup_failed"}, true
	case errors.Is(err, domain.ErrBusy):
		return http.StatusConflict, errorBody{Code: "resource_busy"}, true
	case errors.Is(err, domain.ErrCancelled):
		return http.StatusConflict, errorBody{Code: "operation_cancelled"}, true
	case errors.Is(err, domain.ErrOperationState):
		return http.StatusConflict, errorBody{Code: "operation_state_conflict"}, true
	default:
		return 0, errorBody{}, false
	}
}

// errorHandler renders all HTTP/API failures through the same envelope. Echo
// framework failures (routing, method, body size) are mapped as well as domain
// errors returned by services.
func (h *Handler) errorHandler(c *echo.Context, err error) {
	// The SPA deliberately aborts stale refreshes when a newer event arrives.
	// The client is already gone, so this is neither a 500 nor a useful response.
	if errors.Is(err, context.Canceled) {
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
		logErr := err
		var public *publicError
		if errors.As(err, &public) && public.cause != nil {
			logErr = public.cause
		}
		slog.Error("request failed", "error", logErr, "path", c.Request().URL.Path, "status", status, "code", body.Code)
	}
	_ = c.JSON(status, errorResponse{Error: body})
}

func mapAPIError(err error) (int, errorBody) {
	var public *publicError

	if errors.As(err, &public) {
		return public.status, errorBody{Code: public.code}
	}
	if status, body, ok := mapDomainError(err); ok {
		return status, body
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
