// Package iostest fakes usbmuxd and scripted devices for tests.
package iostest

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"maps"
	"net"
	"slices"
	"sync"
	"testing"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
)

type Muxer struct {
	t       testing.TB
	address string

	mu        sync.Mutex
	nextID    uint32
	attached  map[uint32]attachment
	records   map[string][]byte
	listeners []net.Conn
	down      bool
	hung      bool
}

type attachment struct {
	device     *Device
	connection ios.Connection
}

const BUID = "IOSTEST-BUID"

func NewMuxer(t testing.TB) *Muxer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &Muxer{t: t, address: listener.Addr().String(), attached: map[uint32]attachment{}, records: map[string][]byte{}}
	t.Cleanup(func() {
		_ = listener.Close()
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, conn := range m.listeners {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go m.serve(conn)
		}
	}()
	return m
}

func (m *Muxer) Address() string { return m.address }

func (m *Muxer) Attach(device *Device, connection ios.Connection) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	m.attached[m.nextID] = attachment{device, connection}
	m.announce("Attached", m.nextID)
	return m.nextID
}

func (m *Muxer) Detach(id uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.attached, id)
	m.announce("Detached", id)
}

// SetDown refuses every request and drops listeners, like a stopped usbmuxd.
func (m *Muxer) SetDown(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = down
	if down {
		for _, conn := range m.listeners {
			_ = conn.Close()
		}
		m.listeners = nil
	}
}

// Hang accepts every request and answers none, like a stuck netmuxd.
func (m *Muxer) Hang(hung bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hung = hung
}

func (m *Muxer) Record(udid string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.records[udid]
}

func (m *Muxer) announce(messageType string, id uint32) {
	for _, conn := range m.listeners {
		_ = writeMux(conn, map[string]any{"MessageType": messageType, "DeviceID": id})
	}
}

func (m *Muxer) serve(conn net.Conn) {
	var request map[string]any
	if err := readMux(conn, &request); err != nil {
		_ = conn.Close()
		return
	}
	m.mu.Lock()
	if m.down {
		m.mu.Unlock()
		_ = conn.Close()
		return
	}
	if m.hung {
		m.mu.Unlock()
		_, _ = io.Copy(io.Discard, conn) // until the client gives up
		_ = conn.Close()
		return
	}
	switch request["MessageType"] {
	case "Listen":
		result(conn, 0) // under mu, so no announcement overtakes it
		m.listeners = append(m.listeners, conn)
		m.mu.Unlock()
		return // the conn stays open for announcements
	case "Connect":
		id, _ := request["DeviceID"].(uint64)
		port, _ := request["PortNumber"].(uint64)
		target, attached := m.attached[uint32(id)]
		m.mu.Unlock()
		if !attached {
			result(conn, uint64(ios.MuxBadDevice))
			_ = conn.Close()
			return
		}
		target.device.connect(conn, uint16(port)<<8|uint16(port)>>8)
		return
	}
	reply := m.reply(request)
	m.mu.Unlock()
	_ = writeMux(conn, reply)
	_ = conn.Close()
}

// reply runs with m.mu held.
func (m *Muxer) reply(request map[string]any) map[string]any {
	switch request["MessageType"] {
	case "ListDevices":
		list := []any{}
		for _, id := range slices.Sorted(maps.Keys(m.attached)) {
			entry := m.attached[id]
			list = append(list, map[string]any{"DeviceID": id, "Properties": map[string]any{
				"ConnectionType": string(entry.connection),
				"SerialNumber":   entry.device.UDID,
			}})
		}
		return map[string]any{"DeviceList": list}
	case "ReadBUID":
		return map[string]any{"BUID": BUID}
	case "ReadPairRecord":
		if record, ok := m.records[request["PairRecordID"].(string)]; ok {
			return map[string]any{"PairRecordData": record}
		}
		return map[string]any{"MessageType": "Result", "Number": uint64(ios.MuxBadDevice)}
	case "SavePairRecord":
		m.records[request["PairRecordID"].(string)] = request["PairRecordData"].([]byte)
		return map[string]any{"MessageType": "Result", "Number": uint64(0)}
	}
	return map[string]any{"MessageType": "Result", "Number": uint64(ios.MuxBadCommand)}
}

func result(conn net.Conn, number uint64) {
	_ = writeMux(conn, map[string]any{"MessageType": "Result", "Number": number})
}

// Written apart from package ios, so tests catch mistakes on either side.
func writeMux(w io.Writer, message map[string]any) error {
	body, err := plist.Marshal(message, plist.XMLFormat)
	if err != nil {
		return err
	}
	header := make([]byte, 16, 16+len(body))
	binary.LittleEndian.PutUint32(header[0:], uint32(16+len(body)))
	binary.LittleEndian.PutUint32(header[4:], 1)
	binary.LittleEndian.PutUint32(header[8:], 8)
	binary.LittleEndian.PutUint32(header[12:], 1)
	_, err = w.Write(append(header, body...))
	return err
}

func readMux(r io.Reader, message any) error {
	var header [16]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	body := make([]byte, binary.LittleEndian.Uint32(header[:])-16)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	_, err := plist.Unmarshal(body, message)
	return err
}

type Handler func(conn net.Conn)

type Device struct {
	UDID string

	identity tls.Certificate
	mu       sync.Mutex
	trusted  map[string]bool
	values   map[string]map[string]any
	services map[string]Handler
	ports    map[uint16]string
	nextPort uint16
	dialed   map[string]int
	answer   string         // how the Trust dialog answers Pair; "" trusts
	offered  map[string]any // the PairRecord of the last Pair request
}

func NewDevice(udid string, identity tls.Certificate) *Device {
	return &Device{
		UDID:     udid,
		identity: identity,
		trusted:  map[string]bool{},
		values:   map[string]map[string]any{"": {"DeviceName": "Test iPhone", "ProductType": "iPhone17,1", "ProductVersion": "26.0"}},
		services: map[string]Handler{},
		ports:    map[uint16]string{},
		nextPort: 49152,
		dialed:   map[string]int{},
	}
}

func (d *Device) Trust(hostID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.trusted[hostID] = true
}

func (d *Device) SetValue(domain, key string, value any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.values[domain] == nil {
		d.values[domain] = map[string]any{}
	}
	d.values[domain][key] = value
}

func (d *Device) Value(domain, key string) any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.values[domain][key]
}

// AnswerPairing sets the Trust dialog's answer: "" trusts, otherwise the
// lockdown error to reply with.
func (d *Device) AnswerPairing(answer string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answer = answer
}

func (d *Device) Offered() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.offered
}

func (d *Device) Trusts(hostID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.trusted[hostID]
}

func (d *Device) Dialed(service string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dialed[service]
}

func (d *Device) Handle(service string, handler Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.services[service] = handler
}

func (d *Device) connect(conn net.Conn, port uint16) {
	defer conn.Close()
	if port == ios.LockdownPort {
		result(conn, 0)
		d.serveLockdown(conn)
		return
	}
	d.mu.Lock()
	service := d.ports[port]
	handler := d.services[service]
	d.dialed[service]++
	d.mu.Unlock()
	if handler == nil {
		result(conn, uint64(ios.MuxConnectionRefused))
		return
	}
	result(conn, 0)
	secure := tls.Server(conn, d.tlsConfig())
	defer secure.Close()
	handler(secure)
}

func (d *Device) tlsConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{d.identity}, ClientAuth: tls.RequireAnyClientCert}
}

func (d *Device) serveLockdown(raw net.Conn) {
	conn := ios.NewPlistConn(raw, plist.XMLFormat)
	session := false
	for {
		var request map[string]any
		if err := conn.Recv(&request); err != nil {
			return
		}
		reply := d.lockdown(request, session)
		if err := conn.Send(reply); err != nil {
			return
		}
		if reply["EnableSessionSSL"] == true {
			session = true
			conn = ios.NewPlistConn(tls.Server(raw, d.tlsConfig()), plist.XMLFormat)
		}
	}
}

func (d *Device) lockdown(request map[string]any, session bool) map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	reply := map[string]any{"Request": request["Request"]}
	domain, _ := request["Domain"].(string)
	key, _ := request["Key"].(string)
	switch request["Request"] {
	case "QueryType":
		reply["Type"] = "com.apple.mobile.lockdown"
	case "GetValue":
		switch value, found := d.values[domain][key]; {
		case key == "" && d.values[domain] != nil:
			reply["Value"] = d.values[domain]
		case found:
			reply["Value"] = value
		default:
			reply["Error"] = "MissingValue"
		}
	case "SetValue":
		if d.values[domain] == nil {
			d.values[domain] = map[string]any{}
		}
		d.values[domain][key] = request["Value"]
	case "StartSession":
		if hostID, _ := request["HostID"].(string); !d.trusted[hostID] {
			reply["Error"] = "InvalidHostID"
			break
		}
		reply["SessionID"] = "IOSTEST-SESSION"
		reply["EnableSessionSSL"] = true
	case "StartService":
		name, _ := request["Service"].(string)
		switch {
		case !session:
			reply["Error"] = "SessionInactive"
		case d.services[name] == nil:
			reply["Error"] = "InvalidService"
		default:
			d.nextPort++
			d.ports[d.nextPort] = name
			reply["Port"] = uint64(d.nextPort)
			reply["EnableServiceSSL"] = true
		}
	case "Pair":
		d.offered, _ = request["PairRecord"].(map[string]any)
		hostID, _ := d.offered["HostID"].(string)
		switch {
		case d.answer != "":
			reply["Error"] = d.answer
		case hostID == "":
			reply["Error"] = "InvalidPairRecord"
		default:
			d.trusted[hostID] = true
			reply["EscrowBag"] = []byte("IOSTEST-ESCROW")
		}
	case "Unpair":
		record, _ := request["PairRecord"].(map[string]any)
		hostID, _ := record["HostID"].(string)
		if !d.trusted[hostID] {
			reply["Error"] = "InvalidHostID"
			break
		}
		delete(d.trusted, hostID)
	default:
		reply["Error"] = "UnsupportedRequest"
	}
	return reply
}
