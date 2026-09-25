package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/wizier/airvault/internal/service"
)

// A restore body binds over the defaults: an omitted field keeps the standard
// behavior and an explicit false stays an override.
func TestRestoreBodyBindsOverDefaults(t *testing.T) {
	body := strings.NewReader(`{"snapshotId":"snapshot-1","reboot":false}`)
	request := httptest.NewRequest(http.MethodPost, "/restore", body)
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	opts := service.DefaultRestoreOptions()
	if err := echo.BindBody(echo.New().NewContext(request, httptest.NewRecorder()), &opts); err != nil {
		t.Fatal(err)
	}
	want := service.DefaultRestoreOptions()
	want.SnapshotID, want.Reboot = "snapshot-1", false
	if opts != want {
		t.Fatalf("bound options = %#v, want %#v", opts, want)
	}
}
