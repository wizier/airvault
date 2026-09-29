package ios

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"howett.net/plist"
)

const (
	defaultMuxSocket = "/var/run/usbmuxd"
	muxHeaderSize    = 16
	muxVersionPlist  = 1
	muxMessagePlist  = 8
	maxMuxMessage    = 16 << 20
	muxClient        = "AirVault"
)

// Mux is a usbmuxd-protocol endpoint: usbmuxd, or netmuxd for network devices.
type Mux struct {
	network, address string
}

// ParseMux reads USBMUXD_SOCKET_ADDRESS: empty is the system socket, a value
// without ':' a Unix socket path, anything else host:port.
func ParseMux(address string) (Mux, error) {
	switch {
	case address == "":
		return Mux{"unix", defaultMuxSocket}, nil
	case !strings.Contains(address, ":"):
		return Mux{"unix", address}, nil
	}
	host, port, err := net.SplitHostPort(address)
	if err == nil && host == "" {
		err = errors.New("missing host")
	}
	if err == nil {
		_, err = strconv.ParseUint(port, 10, 16)
	}
	if err != nil {
		return Mux{}, fmt.Errorf("invalid muxer address %q: %w", address, err)
	}
	return Mux{"tcp", address}, nil
}

func (m Mux) String() string { return m.network + ":" + m.address }

type Connection string

const (
	ConnectionUSB     Connection = "USB"
	ConnectionNetwork Connection = "Network"
)

// Device is one attachment; a phone on both USB and Wi-Fi is listed twice.
type Device struct {
	ID         uint32 // the muxer's handle for this attachment
	UDID       string
	Connection Connection
	Addr       netip.Addr // network attachments; invalid when not decodable
}

func (m Mux) Devices(ctx context.Context) ([]Device, error) {
	var reply struct {
		DeviceList []struct {
			DeviceID   uint32 `plist:"DeviceID"`
			Properties struct {
				ConnectionType string `plist:"ConnectionType"`
				SerialNumber   string `plist:"SerialNumber"`
				NetworkAddress []byte `plist:"NetworkAddress"`
			} `plist:"Properties"`
		} `plist:"DeviceList"`
	}
	if err := m.exchange(ctx, muxRequest("ListDevices", nil), &reply); err != nil {
		return nil, err
	}
	devices := make([]Device, 0, len(reply.DeviceList))
	for _, entry := range reply.DeviceList {
		props := entry.Properties
		devices = append(devices, Device{
			ID:         entry.DeviceID,
			UDID:       props.SerialNumber,
			Connection: Connection(props.ConnectionType),
			Addr:       sockaddrIP(props.NetworkAddress),
		})
	}
	return devices, nil
}

// PreferUSB keeps one entry per device, the USB one when there are two.
func PreferUSB(devices []Device) []Device {
	index := make(map[string]int, len(devices))
	unique := make([]Device, 0, len(devices))
	for _, device := range devices {
		i, seen := index[device.UDID]
		switch {
		case !seen:
			index[device.UDID] = len(unique)
			unique = append(unique, device)
		case device.Connection == ConnectionUSB:
			unique[i] = device
		}
	}
	return unique
}

func (m Mux) BUID(ctx context.Context) (string, error) {
	var reply struct {
		BUID string `plist:"BUID"`
	}
	if err := m.exchange(ctx, muxRequest("ReadBUID", nil), &reply); err != nil {
		return "", err
	}
	if reply.BUID == "" {
		return "", fmt.Errorf("%w: ReadBUID without a BUID", ErrProtocol)
	}
	return reply.BUID, nil
}

// PairRecord returns the muxer's own record for udid, a MuxError without one.
func (m Mux) PairRecord(ctx context.Context, udid string) ([]byte, error) {
	var reply struct {
		PairRecordData []byte  `plist:"PairRecordData"`
		Number         *uint64 `plist:"Number"`
	}
	if err := m.exchange(ctx, muxRequest("ReadPairRecord", map[string]any{"PairRecordID": udid}), &reply); err != nil {
		return nil, err
	}
	if len(reply.PairRecordData) == 0 {
		if reply.Number != nil && *reply.Number != 0 {
			return nil, MuxError(*reply.Number)
		}
		return nil, fmt.Errorf("%w: ReadPairRecord without data", ErrProtocol)
	}
	return reply.PairRecordData, nil
}

func (m Mux) SavePairRecord(ctx context.Context, udid string, record []byte) error {
	return m.result(ctx, muxRequest("SavePairRecord", map[string]any{
		"PairRecordID":   udid,
		"PairRecordData": record,
	}))
}

// Connect opens a stream to port on the device; ctx bounds the connect only.
func (m Mux) Connect(ctx context.Context, device Device, port uint16) (net.Conn, error) {
	conn, err := m.dial(ctx)
	if err != nil {
		return nil, err
	}
	_, err = Guard(ctx, conn, func() error {
		// The muxer takes the port in network byte order inside a host-order field.
		return requestResult(conn, muxRequest("Connect", map[string]any{
			"DeviceID":   device.ID,
			"PortNumber": port<<8 | port>>8,
		}))
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("connect to %s port %d: %w", device.UDID, port, err)
	}
	return conn, nil
}

type ListenEvent struct {
	Type     string // "Attached", "Detached" or "Paired"
	DeviceID uint32
}

type Listener struct {
	conn net.Conn
}

// Listen subscribes to attach-state changes; ctx bounds the handshake only.
func (m Mux) Listen(ctx context.Context) (*Listener, error) {
	conn, err := m.dial(ctx)
	if err != nil {
		return nil, err
	}
	_, err = Guard(ctx, conn, func() error { return requestResult(conn, muxRequest("Listen", nil)) })
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("listen: %w", err)
	}
	return &Listener{conn: conn}, nil
}

func (l *Listener) Next(ctx context.Context) (ListenEvent, error) {
	var event struct {
		MessageType string `plist:"MessageType"`
		DeviceID    uint32 `plist:"DeviceID"`
	}
	if _, err := Guard(ctx, l.conn, func() error { return readMux(l.conn, &event) }); err != nil {
		return ListenEvent{}, err
	}
	return ListenEvent{Type: event.MessageType, DeviceID: event.DeviceID}, nil
}

func (l *Listener) Close() error { return l.conn.Close() }

func (m Mux) dial(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, m.network, m.address)
	if err != nil {
		return nil, fmt.Errorf("connect to muxer: %w", err) // the error names the address
	}
	return conn, nil
}

// exchange runs one request on a muxer connection of its own.
func (m Mux) exchange(ctx context.Context, request map[string]any, reply any) error {
	conn, err := m.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = Guard(ctx, conn, func() error {
		if err := writeMux(conn, request); err != nil {
			return err
		}
		return readMux(conn, reply)
	})
	return err
}

func (m Mux) result(ctx context.Context, request map[string]any) error {
	conn, err := m.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = Guard(ctx, conn, func() error { return requestResult(conn, request) })
	return err
}

func requestResult(conn net.Conn, request map[string]any) error {
	if err := writeMux(conn, request); err != nil {
		return err
	}
	var reply struct {
		Number *uint64 `plist:"Number"`
	}
	if err := readMux(conn, &reply); err != nil {
		return err
	}
	switch {
	case reply.Number == nil:
		return fmt.Errorf("%w: result without a Number", ErrProtocol)
	case *reply.Number != 0:
		return MuxError(*reply.Number)
	}
	return nil
}

func muxRequest(messageType string, fields map[string]any) map[string]any {
	request := map[string]any{
		"MessageType":         messageType,
		"ClientVersionString": muxClient,
		"ProgName":            muxClient,
		"kLibUSBMuxVersion":   3,
	}
	maps.Copy(request, fields)
	return request
}

// writeMux frames a plist behind the 16-byte little-endian muxer header.
func writeMux(w io.Writer, request map[string]any) error {
	body, err := plist.Marshal(request, plist.XMLFormat)
	if err != nil {
		return fmt.Errorf("encode muxer request: %w", err)
	}
	packet := make([]byte, muxHeaderSize+len(body))
	binary.LittleEndian.PutUint32(packet[0:], uint32(len(packet)))
	binary.LittleEndian.PutUint32(packet[4:], muxVersionPlist)
	binary.LittleEndian.PutUint32(packet[8:], muxMessagePlist)
	binary.LittleEndian.PutUint32(packet[12:], 1)
	copy(packet[muxHeaderSize:], body)
	_, err = w.Write(packet)
	return err
}

func readMux(r io.Reader, reply any) error {
	var header [muxHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.LittleEndian.Uint32(header[:])
	if size < muxHeaderSize || size > maxMuxMessage {
		return fmt.Errorf("%w: muxer message of %d bytes", ErrProtocol, size)
	}
	body := make([]byte, size-muxHeaderSize)
	if _, err := io.ReadFull(r, body); err != nil {
		return noEOF(err)
	}
	return decodePlist(body, reply)
}

// sockaddrIP decodes a network device's sockaddr. BSD muxers prefix it with
// its length, Linux ones do not; IPv4 sits at bytes 4-8, IPv6 at 8-24.
func sockaddrIP(sockaddr []byte) netip.Addr {
	if len(sockaddr) < 2 {
		return netip.Addr{}
	}
	family := sockaddr[0]
	if family != 2 && family != 10 && family != 30 {
		family = sockaddr[1]
	}
	switch {
	case family == 2 && len(sockaddr) >= 8:
		return netip.AddrFrom4([4]byte(sockaddr[4:8]))
	case (family == 10 || family == 30) && len(sockaddr) >= 24:
		return netip.AddrFrom16([16]byte(sockaddr[8:24]))
	}
	return netip.Addr{}
}
