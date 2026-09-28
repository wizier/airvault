package iostest

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
)

// DeviceLink is the device side of a mobilebackup2 conversation, for tests
// to script; a broken conversation fails t.
type DeviceLink struct {
	t      testing.TB
	conn   net.Conn
	reader *bufio.Reader
}

// Backup2 serves mobilebackup2: the handshake, then script.
func Backup2(t testing.TB, script func(dl *DeviceLink)) Handler {
	return func(conn net.Conn) {
		dl := &DeviceLink{t: t, conn: conn, reader: bufio.NewReader(conn)}
		dl.Send("DLMessageVersionExchange", 300, 0)
		if reply := dl.Recv(); len(reply) < 2 || reply[1] != "DLVersionsOk" {
			t.Errorf("version exchange reply = %v", reply)
			return
		}
		dl.Send("DLMessageDeviceReady")
		if hello := dl.Request(); hello["MessageName"] != "Hello" {
			t.Errorf("expected Hello, got %v", hello)
			return
		}
		dl.Send("DLMessageProcessMessage", map[string]any{"MessageName": "Response", "ErrorCode": 0, "ProtocolVersion": 2.1})
		script(dl)
	}
}

func (dl *DeviceLink) Send(message ...any) {
	body, err := plist.Marshal(message, plist.BinaryFormat)
	if err == nil {
		err = ios.WriteFrame(dl.conn, body)
	}
	if err != nil {
		dl.t.Errorf("device send: %v", err)
	}
}

func (dl *DeviceLink) Recv() []any {
	body, err := ios.ReadFrame(dl.reader)
	var message []any
	if err == nil {
		_, err = plist.Unmarshal(body, &message)
	}
	if err != nil {
		dl.t.Errorf("device recv: %v", err)
	}
	return message
}

// Wait returns once the host says DLMessageDisconnect, after which a device
// hangs up, or hangs up itself; farewell reports which.
func (dl *DeviceLink) Wait() (farewell bool) {
	for {
		body, err := ios.ReadFrame(dl.reader)
		if err != nil {
			return false
		}
		var message []any
		if _, err := plist.Unmarshal(body, &message); err == nil && len(message) > 0 && message[0] == "DLMessageDisconnect" {
			return true
		}
	}
}

// Request reads the host's DLMessageProcessMessage.
func (dl *DeviceLink) Request() map[string]any {
	message := dl.Recv()
	if len(message) < 2 || message[0] != "DLMessageProcessMessage" {
		dl.t.Errorf("expected a request, got %v", message)
		return nil
	}
	request, _ := message[1].(map[string]any)
	return request
}

// Ask sends a DeviceLink message and returns the host's status code and value.
func (dl *DeviceLink) Ask(message ...any) (int64, any) {
	dl.Send(message...)
	return dl.Status()
}

func (dl *DeviceLink) Status() (int64, any) {
	message := dl.Recv()
	if len(message) < 4 || message[0] != "DLMessageStatusResponse" {
		dl.t.Errorf("expected a status response, got %v", message)
		return 0, nil
	}
	code, _ := message[1].(int64)
	if unsigned, ok := message[1].(uint64); ok {
		code = int64(unsigned)
	}
	return code, message[3]
}

// UploadFile is one file of an upload. A Trailer ends it with an error
// block, which devices send routinely after the data; Cut drops the
// connection after the data instead.
type UploadFile struct {
	Path    string
	Data    []byte
	Trailer string
	Cut     bool
}

// Upload sends files as a backup does, reaching target percent, and
// returns the host's status code, -1 once a file cut it short.
func (dl *DeviceLink) Upload(target float64, files ...UploadFile) int64 {
	var total uint64
	for _, file := range files {
		total += uint64(len(file.Data))
	}
	dl.Send("DLMessageUploadFiles", map[string]any{}, target, total)
	for _, file := range files {
		dl.writeString(file.Path) // the directory, which hosts ignore
		dl.writeString(file.Path)
		for data := file.Data; len(data) > 0; {
			chunk := data[:min(len(data), 1000)]
			dl.writeBlock(0x0C, chunk)
			data = data[len(chunk):]
		}
		switch {
		case file.Cut:
			_ = dl.conn.Close()
			return -1
		case file.Trailer != "":
			dl.writeBlock(0x0B, []byte(file.Trailer))
		default:
			dl.writeBlock(0x00, nil)
		}
	}
	dl.writeU32(0)
	code, _ := dl.Status()
	return code
}

// Download asks for files as a restore does; it returns what arrived and
// the host's status code and value.
func (dl *DeviceLink) Download(target float64, paths ...string) (map[string][]byte, int64, any) {
	list := make([]any, len(paths))
	for i, path := range paths {
		list[i] = path
	}
	dl.Send("DLMessageDownloadFiles", list, map[string]any{}, target)
	received := map[string][]byte{}
	for {
		name := dl.readBlock()
		if name == nil {
			break
		}
		var data []byte
		for {
			block := dl.readBlock()
			if len(block) == 0 || block[0] != 0x0C {
				if len(block) > 0 && block[0] == 0x00 {
					received[string(name)] = data
				}
				break
			}
			data = append(data, block[1:]...)
		}
	}
	code, value := dl.Status()
	return received, code, value
}

// Finish ends the transfer with the device's verdict.
func (dl *DeviceLink) Finish(errorCode int, description string) {
	dl.Send("DLMessageProcessMessage", map[string]any{"MessageName": "Response", "ErrorCode": errorCode, "ErrorDescription": description})
}

func (dl *DeviceLink) writeString(s string) {
	dl.writeU32(uint32(len(s)))
	dl.write([]byte(s))
}

func (dl *DeviceLink) writeBlock(code byte, data []byte) {
	dl.writeU32(uint32(1 + len(data)))
	dl.write(append([]byte{code}, data...))
}

func (dl *DeviceLink) writeU32(v uint32) { dl.write(binary.BigEndian.AppendUint32(nil, v)) }

func (dl *DeviceLink) write(p []byte) {
	if _, err := dl.conn.Write(p); err != nil {
		dl.t.Errorf("device write: %v", err)
	}
}

// readBlock reads a length-prefixed block, nil for a zero length.
func (dl *DeviceLink) readBlock() []byte {
	var n uint32
	if err := binary.Read(dl.reader, binary.BigEndian, &n); err != nil || n == 0 {
		return nil
	}
	block := make([]byte, n)
	if _, err := io.ReadFull(dl.reader, block); err != nil {
		dl.t.Errorf("device read block: %v", err)
		return nil
	}
	return block
}
