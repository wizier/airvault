package handler

import (
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

const (
	writeTimeout = time.Minute
	// A quiet stream still sends a comment this often so proxies don't reap it.
	streamPingInterval = 25 * time.Second
)

// writeDeadlines bounds each write, so a client that stops reading cannot hold
// a download's device lease forever. Keep-alive reuses the connection, so a
// request starts and ends without a deadline.
func writeDeadlines(timeout time.Duration) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			w := &deadlineWriter{ResponseWriter: c.Response(), controller: http.NewResponseController(c.Response()), timeout: timeout}
			_ = w.controller.SetWriteDeadline(time.Time{})
			defer func() { _ = w.controller.SetWriteDeadline(time.Time{}) }()
			c.SetResponse(w)
			return next(c)
		}
	}
}

type deadlineWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	_ = w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
	return w.ResponseWriter.Write(p)
}

func (w *deadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type responseStream struct {
	controller *http.ResponseController
	writer     io.Writer
}

func startStream(c *echo.Context, contentType string) *responseStream {
	res := c.Response()
	res.Header().Set(echo.HeaderContentType, contentType)
	res.Header().Set("Cache-Control", "no-cache")
	res.Header().Set("X-Accel-Buffering", "no")
	return &responseStream{controller: http.NewResponseController(res), writer: res}
}

func (s *responseStream) write(frame string) error {
	if _, err := io.WriteString(s.writer, frame); err != nil {
		return err
	}
	return s.controller.Flush()
}
