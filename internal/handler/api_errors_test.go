package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/wizier/airvault/internal/domain"
)

func TestAPIErrorPayloadContainsCodeOnly(t *testing.T) {
	_, body := mapAPIError(domain.ErrBusy)
	payload, err := json.Marshal(errorResponse{Error: body})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(payload), `{"error":{"code":"resource_busy"}}`; got != want {
		t.Fatalf("error payload = %s, want %s", got, want)
	}
}

func TestValidationErrorUsesStableCodeWithoutDiagnostic(t *testing.T) {
	status, body := mapAPIError(&domain.ValidationError{
		Code: "invalid_path", Message: "native path parser detail",
	})
	if status != http.StatusUnprocessableEntity || body.Code != "invalid_path" {
		t.Fatalf("validation error = (%d, %#v), want 422 invalid_path", status, body)
	}
	payload, err := json.Marshal(errorResponse{Error: body})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(payload), `{"error":{"code":"invalid_path"}}`; got != want {
		t.Fatalf("error payload = %s, want %s", got, want)
	}
}

// A cancellation-shaped cause (a closed AFC slot) still gets a response while
// the client is connected; only a client that went away gets none.
func TestErrorHandlerAnswersUnlessClientIsGone(t *testing.T) {
	err := domain.NewActionError("stat_failed", context.Canceled)

	live := httptest.NewRecorder()
	(&Handler{}).errorHandler(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), live), err)
	if live.Code != http.StatusConflict || !strings.Contains(live.Body.String(), `"stat_failed"`) {
		t.Fatalf("live request got %d %q, want 409 stat_failed", live.Code, live.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gone := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	(&Handler{}).errorHandler(echo.New().NewContext(request, gone), err)
	if gone.Body.Len() != 0 {
		t.Fatalf("gone client got body %q", gone.Body.String())
	}
}
