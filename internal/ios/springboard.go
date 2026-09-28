package ios

import (
	"context"
	"fmt"
	"net"

	"howett.net/plist"
)

const SpringBoardService = "com.apple.springboardservices"

type SpringBoard struct {
	conn *PlistConn
}

func NewSpringBoard(conn net.Conn) *SpringBoard {
	return &SpringBoard{conn: NewPlistConn(conn, plist.XMLFormat)}
}

func (s *SpringBoard) Close() error { return s.conn.Close() }

func (s *SpringBoard) IconPNG(ctx context.Context, bundleID string) ([]byte, error) {
	return s.png(ctx, map[string]any{"command": "getIconPNGData", "bundleId": bundleID})
}

func (s *SpringBoard) WallpaperPNG(ctx context.Context, lockScreen bool) ([]byte, error) {
	name := "homescreen"
	if lockScreen {
		name = "lockscreen"
	}
	return s.png(ctx, map[string]any{"command": "getWallpaperPreviewImage", "wallpaperName": name})
}

func (s *SpringBoard) png(ctx context.Context, request map[string]any) ([]byte, error) {
	var reply struct {
		PNGData []byte `plist:"pngData"`
	}
	if err := s.conn.Exchange(ctx, request, &reply); err != nil {
		return nil, fmt.Errorf("springboard %s: %w", request["command"], err)
	}
	if reply.PNGData == nil {
		return nil, fmt.Errorf("springboard %s: %w: no pngData", request["command"], ErrProtocol)
	}
	return reply.PNGData, nil
}
