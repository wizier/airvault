package ios

import (
	"context"
	"net"
	"time"
)

var expired = time.Unix(1, 0)

// Guard runs exchange on conn and interrupts it once ctx ends: then it returns
// ctx's error and torn, as conn may be left mid-message.
func Guard(ctx context.Context, conn net.Conn, exchange func() error) (torn bool, err error) {
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(expired) })
	err = exchange()
	if !stop() {
		return true, ctx.Err()
	}
	return false, err
}
