package handler

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

// [GET] /api/devices/:udid/console
func (h *Handler) streamDeviceConsole(c *echo.Context) error {
	udid := c.Param("udid")
	rc := startStream(c, "text/event-stream")
	res := c.Response()

	// The device can go quiet for long stretches, so pings keep the stream
	// alive. Writes come from two goroutines (records + pings), so they
	// serialize on a mutex.
	var wmu sync.Mutex
	write := func(payload string) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeStreamFrame(rc, res, payload)
	}
	if err := write(": connected\n\n"); err != nil {
		return nil
	}

	ctx, cancel := context.WithCancel(c.Request().Context())
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ping := time.NewTicker(streamPingInterval)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				if err := write(": ping\n\n"); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	// Join the pinger before returning so every write stays inside ServeHTTP.
	defer func() { cancel(); <-pingDone }()

	err := h.svc.Console(ctx, udid, func(line service.ConsoleLine) {
		if ctx.Err() != nil {
			return
		}
		data, _ := json.Marshal(line) // strings and a uint32 cannot fail
		if err := write("data: " + string(data) + "\n\n"); err != nil {
			cancel()
		}
	})
	if err != nil && ctx.Err() == nil {
		// Headers are committed — deliver the failure in-band; the console UI
		// listens for this event and stops reconnecting.
		_, body := mapAPIError(err)
		data, _ := json.Marshal(body)
		_ = write("event: error\ndata: " + string(data) + "\n\n")
	}
	return nil
}
