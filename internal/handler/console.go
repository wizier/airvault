package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/engine"

	"github.com/labstack/echo/v5"
)

// [GET] /api/devices/:udid/console
func (h *Handler) streamDeviceConsole(c *echo.Context) error {
	udid := c.Param("udid")
	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(res)
	ctx, cancel := context.WithCancel(c.Request().Context())
	defer cancel()

	// The device can go quiet for long stretches; a heartbeat keeps proxies
	// from reaping the stream. Writes come from two goroutines (records +
	// pings), so they serialize on a mutex.
	var wmu sync.Mutex
	write := func(payload string) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeStreamFrame(rc, res, payload)
	}
	if err := write(": connected\n\n"); err != nil {
		return nil
	}

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	stopPing := make(chan struct{})
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		for {
			select {
			case <-stopPing:
				return
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
	defer func() { close(stopPing); <-pingDone }()

	err := h.svc.Console(ctx, udid, func(line engine.ConsoleLine) {
		if ctx.Err() != nil {
			return
		}
		data, jerr := json.Marshal(line)
		if jerr != nil {
			return
		}
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
