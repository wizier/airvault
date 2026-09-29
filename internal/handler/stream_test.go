package handler

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

// A client that stops reading costs a handler one write timeout, and a
// connection whose deadline passed still serves the next request.
func TestWriteDeadlines(t *testing.T) {
	const timeout = 100 * time.Millisecond
	e := echo.New()
	e.Use(writeDeadlines(timeout))
	stalled := make(chan error, 1)
	e.GET("/endless", func(c *echo.Context) error {
		chunk := make([]byte, 64<<10)
		for {
			if _, err := c.Response().Write(chunk); err != nil {
				stalled <- err
				return nil
			}
		}
	})
	e.GET("/text", func(c *echo.Context) error { return c.String(http.StatusOK, "text") })
	e.GET("/empty", func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) })
	server := httptest.NewServer(e)
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET /endless HTTP/1.1\r\nHost: test\r\n\r\n")
	select {
	case <-stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("a write to a client that stopped reading never timed out")
	}

	client := server.Client()
	get := func(path string) {
		t.Helper()
		response, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	get("/text")
	time.Sleep(2 * timeout)
	get("/empty")
}
