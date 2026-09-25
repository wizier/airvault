package handler

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/wizier/airvault/internal/events"
)

// [GET] /api/events
func (h *Handler) streamEvents(c *echo.Context) error {
	stream := startStream(c, "text/event-stream")
	// IDs are "<epoch>-<seq>"; a Last-Event-ID from another process (epoch
	// mismatch) has no replayable history here and must trigger a resync.
	lastEventID := c.Request().Header.Get("Last-Event-ID")
	var afterID uint64
	staleEpoch := lastEventID != ""
	if epoch, seq, ok := strings.Cut(lastEventID, "-"); ok && epoch == h.bus.Epoch() {
		if parsed, err := strconv.ParseUint(seq, 10, 64); err == nil {
			afterID = parsed
			staleEpoch = false
		}
	}
	id, ch, replay, complete := h.bus.Subscribe(afterID)
	defer h.bus.Unsubscribe(id)

	initial := "retry: 2000\n: connected\n\n"
	if !complete || staleEpoch {
		initial += "event: " + events.StreamReset + "\ndata: {}\n\n"
	}
	if err := stream.write(initial); err != nil {
		return nil
	}
	for _, event := range replay {
		if err := writeEvent(stream, h.bus.Epoch(), event); err != nil {
			return nil
		}
	}

	ctx := c.Request().Context()
	ping := time.NewTicker(streamPingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ping.C:
			if err := stream.write(": ping\n\n"); err != nil {
				return nil
			}
		case event, ok := <-ch:
			if !ok {
				return nil
			}
			if err := writeEvent(stream, h.bus.Epoch(), event); err != nil {
				return nil
			}
		}
	}
}

func writeEvent(stream *responseStream, epoch string, event events.Event) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		data = []byte("null")
	}
	frame := fmt.Sprintf("id: %s-%d\nevent: %s\ndata: %s\n\n", epoch, event.ID, event.Type, data)
	return stream.write(frame)
}
