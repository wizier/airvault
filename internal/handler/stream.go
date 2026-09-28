package handler

import (
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

const (
	// A stream may live indefinitely, but an individual write must not hold its
	// request goroutine forever when a client stops reading without disconnecting.
	streamWriteTimeout = 15 * time.Second
	// A quiet stream still sends a comment this often so proxies don't reap it.
	streamPingInterval = 25 * time.Second
)

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

func (s *responseStream) write(frame string) (err error) {
	if err := s.controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil {
		return err
	}
	defer func() {
		if resetErr := s.controller.SetWriteDeadline(time.Time{}); err == nil {
			err = resetErr
		}
	}()
	if _, err := io.WriteString(s.writer, frame); err != nil {
		return err
	}
	return s.controller.Flush()
}
