package handler

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

func TestStreamInstallFraming(t *testing.T) {
	progress := []service.InstallProgress{
		{Phase: "staging", Percent: 40},
		{Phase: "installing", Percent: 100},
	}
	progressLines := `{"phase":"staging","percent":40}` + "\n" + `{"phase":"installing","percent":100}` + "\n"
	for _, tc := range []struct {
		name        string
		progress    []service.InstallProgress
		result      error
		status      int
		contentType string
		body        string
	}{
		{
			name: "success", progress: progress, status: http.StatusOK, contentType: "application/x-ndjson",
			body: progressLines + `{"done":true}` + "\n",
		},
		{
			name: "failure after progress", progress: progress, result: domain.NewActionError("app_install_failed", errors.New("boom")),
			status: http.StatusOK, contentType: "application/x-ndjson",
			body: progressLines + `{"error":{"code":"app_install_failed"}}` + "\n",
		},
		{
			name: "failure before progress", result: domain.ErrDeviceOffline,
			status: http.StatusConflict, contentType: "application/json",
			body: `{"error":{"code":"device_offline"}}` + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			e.HTTPErrorHandler = (&Handler{}).errorHandler
			e.POST("/install", func(c *echo.Context) error {
				return streamInstall(c, func(onProgress func(service.InstallProgress)) error {
					for _, p := range tc.progress {
						onProgress(p)
					}
					return tc.result
				})
			})
			server := httptest.NewServer(e)
			defer server.Close()

			res, err := http.Post(server.URL+"/install", "application/octet-stream", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != tc.status || res.Header.Get("Content-Type") != tc.contentType {
				t.Fatalf("got %d %q, want %d %q", res.StatusCode, res.Header.Get("Content-Type"), tc.status, tc.contentType)
			}
			if string(body) != tc.body {
				t.Fatalf("body = %q, want %q", body, tc.body)
			}
		})
	}
}
