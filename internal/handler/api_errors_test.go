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

// The wire payload carries only the stable code, never a diagnostic.
func TestAPIErrorPayloadContainsCodeOnly(t *testing.T) {
	for _, test := range []struct {
		err         error
		wantStatus  int
		wantPayload string
	}{
		{domain.ErrBusy, http.StatusConflict, `{"error":{"code":"resource_busy"}}`},
		{&domain.ValidationError{Code: "invalid_path", Message: "native path parser detail"},
			http.StatusUnprocessableEntity, `{"error":{"code":"invalid_path"}}`},
	} {
		status, body := mapAPIError(test.err)
		payload, err := json.Marshal(errorResponse{Error: body})
		if err != nil {
			t.Fatal(err)
		}
		if status != test.wantStatus || string(payload) != test.wantPayload {
			t.Errorf("mapAPIError(%v) = %d %s, want %d %s", test.err, status, payload, test.wantStatus, test.wantPayload)
		}
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
