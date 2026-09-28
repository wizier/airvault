package ios

import (
	"context"
	"net"
	"time"
)

var expired = time.Unix(1, 0)

// Bind interrupts conn's I/O once ctx ends. release reports whether conn is
// intact; an interrupted one may be torn mid-message. Only ctx's end expires
// conn: a copied deadline could fire first and race that verdict.
func Bind(ctx context.Context, conn net.Conn) (release func() (intact bool)) {
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(expired) })
	return stop
}
