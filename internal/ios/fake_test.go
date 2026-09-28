package ios

import (
	"net"
	"testing"
)

// startMux serves a fake muxer on loopback TCP: each accepted connection's
// first request goes to handle, which answers on conn (and may keep it).
func startMux(t *testing.T, handle func(conn net.Conn, request map[string]any)) Mux {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				var request map[string]any
				if err := readMux(conn, &request); err != nil {
					_ = conn.Close()
					return
				}
				handle(conn, request)
			}()
		}
	}()
	return Mux{"tcp", listener.Addr().String()}
}

func muxResult(conn net.Conn, number uint64) {
	_ = writeMux(conn, map[string]any{"MessageType": "Result", "Number": number})
}
