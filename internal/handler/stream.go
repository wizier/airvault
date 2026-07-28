package handler

import (
	"io"
	"net/http"
	"time"
)

// A stream may live indefinitely, but an individual write must not hold its
// request goroutine forever when a client stops reading without disconnecting.
const streamWriteTimeout = 15 * time.Second

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
