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

// startSSE sets the event-stream headers and returns the controller that
// flushes each frame.
func startSSE(c *echo.Context) *http.ResponseController {
	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.Header().Set("X-Accel-Buffering", "no")
	return http.NewResponseController(res)
}

func writeStreamFrame(controller *http.ResponseController, writer io.Writer, frame string) (err error) {
	if err := controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil {
		return err
	}
	defer func() {
		if resetErr := controller.SetWriteDeadline(time.Time{}); err == nil {
			err = resetErr
		}
	}()
	if _, err := io.WriteString(writer, frame); err != nil {
		return err
	}
	return controller.Flush()
}
