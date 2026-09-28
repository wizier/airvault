package ios

import (
	"context"
	"errors"
	"fmt"
	"net"

	"howett.net/plist"
)

const NotificationProxyService = "com.apple.mobile.notification_proxy"

var ErrProxyDeath = errors.New("notification proxy closed the connection")

type NotificationProxy struct {
	conn *PlistConn
}

func NewNotificationProxy(conn net.Conn) *NotificationProxy {
	return &NotificationProxy{conn: NewPlistConn(conn, plist.XMLFormat)}
}

func (n *NotificationProxy) Close() error { return n.conn.Close() }

func (n *NotificationProxy) Observe(ctx context.Context, names ...string) error {
	return n.send(ctx, "ObserveNotification", names)
}

func (n *NotificationProxy) Post(ctx context.Context, names ...string) error {
	return n.send(ctx, "PostNotification", names)
}

func (n *NotificationProxy) send(ctx context.Context, command string, names []string) error {
	for _, name := range names {
		if err := n.conn.SendContext(ctx, map[string]any{"Command": command, "Name": name}); err != nil {
			return fmt.Errorf("%s %s: %w", command, name, err)
		}
	}
	return nil
}

func (n *NotificationProxy) Next(ctx context.Context) (string, error) {
	var message struct {
		Command string `plist:"Command"`
		Name    string `plist:"Name"`
	}
	if err := n.conn.RecvContext(ctx, &message); err != nil {
		return "", err
	}
	switch message.Command {
	case "RelayNotification":
		return message.Name, nil
	case "ProxyDeath":
		return "", ErrProxyDeath
	}
	return "", fmt.Errorf("%w: notification proxy sent %q", ErrProtocol, message.Command)
}
