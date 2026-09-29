package ios

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"howett.net/plist"
)

const LockdownPort = 62078

const lockdownLabel = "AirVault"

// Lockdown is a lockdownd conversation, plaintext until StartSession.
type Lockdown struct {
	conn *PlistConn
}

// DialLockdown connects to lockdownd; ctx bounds the connect only.
func DialLockdown(ctx context.Context, mux Mux, device Device) (*Lockdown, error) {
	conn, err := mux.Connect(ctx, device, LockdownPort)
	if err != nil {
		return nil, err
	}
	return &Lockdown{conn: NewPlistConn(conn, plist.XMLFormat)}, nil
}

func (l *Lockdown) Close() error { return l.conn.Close() }

func (l *Lockdown) request(ctx context.Context, request map[string]any, reply any) error {
	request["Label"] = lockdownLabel
	return l.conn.Exchange(ctx, request, reply)
}

func (l *Lockdown) QueryType(ctx context.Context) (string, error) {
	var reply struct {
		Type string `plist:"Type"`
	}
	err := l.request(ctx, map[string]any{"Request": "QueryType"}, &reply)
	return reply.Type, err
}

// Value reads key in domain into a T; an empty key reads the whole domain.
func (l *Lockdown) Value[T any](ctx context.Context, domain, key string) (T, error) {
	var reply struct {
		Value *T `plist:"Value"`
	}
	request := map[string]any{"Request": "GetValue"}
	setIf(request, "Domain", domain)
	setIf(request, "Key", key)
	if err := l.request(ctx, request, &reply); err != nil {
		var zero T
		return zero, fmt.Errorf("read %s/%s: %w", domain, key, err)
	}
	if reply.Value == nil {
		var zero T
		return zero, fmt.Errorf("read %s/%s: %w: no Value", domain, key, ErrProtocol)
	}
	return *reply.Value, nil
}

func (l *Lockdown) SetValue(ctx context.Context, domain, key string, value any) error {
	request := map[string]any{"Request": "SetValue", "Key": key, "Value": value}
	setIf(request, "Domain", domain)
	if err := l.request(ctx, request, &struct{}{}); err != nil {
		return fmt.Errorf("write %s/%s: %w", domain, key, err)
	}
	return nil
}

// StartSession moves to TLS; a device that does not know record answers
// ErrInvalidHostID.
func (l *Lockdown) StartSession(ctx context.Context, record *PairRecord) error {
	config, err := record.TLSConfig()
	if err != nil {
		return err
	}
	var reply struct {
		EnableSessionSSL bool `plist:"EnableSessionSSL"`
	}
	if err := l.request(ctx, map[string]any{
		"Request":    "StartSession",
		"HostID":     record.HostID,
		"SystemBUID": record.SystemBUID,
	}, &reply); err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	if !reply.EnableSessionSSL {
		return fmt.Errorf("start session: %w: session without SSL", ErrProtocol)
	}
	conn, err := handshake(ctx, l.conn.Conn, config)
	if err != nil {
		l.conn.torn = true
		return fmt.Errorf("start session: %w", err)
	}
	l.conn = NewPlistConn(conn, plist.XMLFormat)
	return nil
}

type Service struct {
	Name string
	Port uint16
	SSL  bool // the service connection must be TLS with the pairing record
}

// StartService starts name; escrowBag unlocks data protection for backups.
func (l *Lockdown) StartService(ctx context.Context, name string, escrowBag []byte) (Service, error) {
	request := map[string]any{"Request": "StartService", "Service": name}
	if escrowBag != nil {
		request["EscrowBag"] = escrowBag
	}
	var reply struct {
		Port             uint16 `plist:"Port"`
		EnableServiceSSL bool   `plist:"EnableServiceSSL"`
	}
	if err := l.request(ctx, request, &reply); err != nil {
		return Service{}, fmt.Errorf("start %s: %w", name, err)
	}
	if reply.Port == 0 {
		return Service{}, fmt.Errorf("start %s: %w: no Port", name, ErrProtocol)
	}
	return Service{Name: name, Port: reply.Port, SSL: reply.EnableServiceSSL}, nil
}

// DialService connects to a started service, over TLS when it asks for it.
func DialService(ctx context.Context, mux Mux, device Device, service Service, record *PairRecord) (net.Conn, error) {
	conn, err := mux.Connect(ctx, device, service.Port)
	if err != nil {
		return nil, err
	}
	if !service.SSL {
		return conn, nil
	}
	config, err := record.TLSConfig()
	var secure net.Conn
	if err == nil {
		secure, err = handshake(ctx, conn, config)
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%s: %w", service.Name, err)
	}
	return secure, nil
}

func handshake(ctx context.Context, conn net.Conn, config *tls.Config) (net.Conn, error) {
	tlsConn := tls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	return tlsConn, nil
}

func setIf(request map[string]any, key, value string) {
	if value != "" {
		request[key] = value
	}
}
