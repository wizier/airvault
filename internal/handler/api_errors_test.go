package handler

import (
	"encoding/json"
	"net/http"
	"testing"

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
