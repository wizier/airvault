package iostest

import (
	"encoding/binary"
	"io"
	"maps"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
)

// FS is an in-memory device filesystem served over AFC.
type FS struct {
	mu      sync.Mutex
	files   map[string][]byte
	dirs    map[string]bool
	locked  map[string]bool
	hang    map[string]bool
	full    bool
	nextFD  uint64
	handles map[uint64]*handle
}

type handle struct {
	path   string
	offset int
	locked bool
}

const (
	afcSuccess        = 0
	afcObjectNotFound = 8
	afcObjectIsDir    = 9
	afcNoSpaceLeft    = 18
	afcOpWouldBlock   = 19
)

func NewFS() *FS {
	return &FS{files: map[string][]byte{}, dirs: map[string]bool{"": true}, locked: map[string]bool{},
		hang: map[string]bool{}, handles: map[uint64]*handle{}}
}

func clean(p string) string { return strings.TrimPrefix(path.Clean("/"+p), "/") }

func (fs *FS) WriteFile(name string, data []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	name = clean(name)
	for dir := path.Dir(name); dir != "." && !fs.dirs[dir]; dir = path.Dir(dir) {
		fs.dirs[dir] = true
	}
	fs.files[name] = data
}

func (fs *FS) ReadFile(name string) ([]byte, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	data, ok := fs.files[clean(name)]
	return data, ok
}

func (fs *FS) Exists(name string) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	name = clean(name)
	_, file := fs.files[name]
	return file || fs.dirs[name]
}

// Hang makes reads of name block, like a phone that stopped answering.
func (fs *FS) Hang(name string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.hang[clean(name)] = true
}

func (fs *FS) SetFull(full bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.full = full
}

func (fs *FS) Lock(name string, held bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.locked[clean(name)] = held
}

func (fs *FS) OpenHandles() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.handles)
}

func (fs *FS) Serve(conn net.Conn) {
	for {
		op, header, payload, err := readAFC(conn)
		if err != nil {
			return
		}
		fs.mu.Lock()
		hang := op == 0x0F && fs.handles[binary.LittleEndian.Uint64(header)] != nil &&
			fs.hang[fs.handles[binary.LittleEndian.Uint64(header)].path]
		fs.mu.Unlock()
		if hang {
			_, _ = io.Copy(io.Discard, conn) // until the client gives up
			return
		}
		replyOp, replyHeader, replyPayload := fs.answer(op, header, payload)
		if writeAFC(conn, replyOp, replyHeader, replyPayload) != nil {
			return
		}
	}
}

func (fs *FS) answer(op uint64, header, payload []byte) (uint64, []byte, []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	le := binary.LittleEndian
	status := func(code uint64) (uint64, []byte, []byte) { return 0x01, le.AppendUint64(nil, code), nil }
	switch op {
	case 0x03: // ReadDir
		dir := clean(string(header))
		if !fs.dirs[dir] {
			return status(afcObjectNotFound)
		}
		names := append([]string{".", ".."}, slices.Sorted(maps.Keys(fs.children(dir)))...)
		return 0x02, nil, []byte(strings.Join(names, "\x00") + "\x00")
	case 0x0A: // GetFileInfo
		name := clean(string(header))
		info := []string{"st_mtime", "1790000000000000000", "st_blocks", "8", "st_nlink", "1"}
		switch data, file := fs.files[name]; {
		case file:
			info = append(info, "st_size", strconv.Itoa(len(data)), "st_ifmt", "S_IFREG")
		case fs.dirs[name]:
			info = append(info, "st_size", "64", "st_ifmt", "S_IFDIR")
		default:
			return status(afcObjectNotFound)
		}
		return 0x02, nil, []byte(strings.Join(info, "\x00") + "\x00")
	case 0x08: // RemovePath
		name := clean(string(header))
		if _, file := fs.files[name]; !file && !fs.dirs[name] {
			return status(afcObjectNotFound)
		}
		delete(fs.files, name)
		delete(fs.dirs, name)
		return status(afcSuccess)
	case 0x22: // RemovePathAndContents
		name := clean(string(header))
		if _, file := fs.files[name]; !file && !fs.dirs[name] {
			return status(afcObjectNotFound)
		}
		below := func(p string) bool { return p == name || strings.HasPrefix(p, name+"/") }
		maps.DeleteFunc(fs.dirs, func(dir string, _ bool) bool { return below(dir) })
		maps.DeleteFunc(fs.files, func(file string, _ []byte) bool { return below(file) })
		return status(afcSuccess)
	case 0x09: // MakeDir
		fs.dirs[clean(string(header))] = true
		return status(afcSuccess)
	case 0x0D: // FileOpen
		mode, name := le.Uint64(header), clean(string(header[8:]))
		if fs.dirs[name] {
			return status(afcObjectIsDir)
		}
		if _, exists := fs.files[name]; !exists {
			if mode == 1 {
				return status(afcObjectNotFound)
			}
			fs.files[name] = nil
		}
		if mode == 3 {
			fs.files[name] = nil
		}
		fs.nextFD++
		fs.handles[fs.nextFD] = &handle{path: name}
		return 0x0E, le.AppendUint64(nil, fs.nextFD), nil
	}
	h := fs.handles[le.Uint64(header)]
	if h == nil {
		return status(7) // invalid argument
	}
	switch op {
	case 0x0F: // Read
		data := fs.files[h.path][min(h.offset, len(fs.files[h.path])):]
		n := min(len(data), int(le.Uint64(header[8:])))
		h.offset += n
		return 0x02, nil, data[:n]
	case 0x10: // Write
		if fs.full {
			return status(afcNoSpaceLeft)
		}
		fs.files[h.path] = append(fs.files[h.path][:h.offset], payload...)
		h.offset += len(payload)
		return status(afcSuccess)
	case 0x11: // Seek (SEEK_SET)
		h.offset = int(le.Uint64(header[16:]))
		return status(afcSuccess)
	case 0x1B: // Lock
		if le.Uint64(header[8:]) == 12 {
			if h.locked {
				fs.locked[h.path], h.locked = false, false
			}
			return status(afcSuccess)
		}
		if fs.locked[h.path] && !h.locked {
			return status(afcOpWouldBlock) // flock with LOCK_NB
		}
		fs.locked[h.path], h.locked = true, true
		return status(afcSuccess)
	case 0x14: // Close
		if h.locked {
			fs.locked[h.path] = false
		}
		delete(fs.handles, le.Uint64(header))
		return status(afcSuccess)
	}
	return status(15) // not supported
}

func (fs *FS) children(dir string) map[string]bool {
	children := map[string]bool{}
	for name := range fs.files {
		if path.Dir(name) == dir || dir == "" && !strings.Contains(name, "/") {
			children[path.Base(name)] = true
		}
	}
	for name := range fs.dirs {
		if name != "" && (path.Dir(name) == dir || dir == "" && !strings.Contains(name, "/")) {
			children[path.Base(name)] = true
		}
	}
	return children
}

// AFC framing, written independently of package afc.
func readAFC(r io.Reader) (op uint64, header, payload []byte, err error) {
	var head [40]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, nil, err
	}
	le := binary.LittleEndian
	body := make([]byte, le.Uint64(head[8:])-40)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, nil, err
	}
	split := le.Uint64(head[16:]) - 40
	return le.Uint64(head[32:]), body[:split], body[split:], nil
}

func writeAFC(w io.Writer, op uint64, header, payload []byte) error {
	le := binary.LittleEndian
	out := le.AppendUint64(nil, 0x4141504c36414643)
	out = le.AppendUint64(out, uint64(40+len(header)+len(payload)))
	out = le.AppendUint64(out, uint64(40+len(header)))
	out = le.AppendUint64(out, 0)
	out = le.AppendUint64(out, op)
	_, err := w.Write(append(append(out, header...), payload...))
	return err
}

// HouseArrest vends the Documents of the apps in apps.
func HouseArrest(apps map[string]*FS) Handler {
	return func(raw net.Conn) {
		conn := ios.NewPlistConn(raw, plist.XMLFormat)
		var request map[string]any
		if conn.Recv(&request) != nil {
			return
		}
		bundleID, _ := request["Identifier"].(string)
		documents := apps[bundleID]
		if request["Command"] != "VendDocuments" || documents == nil {
			_ = conn.Send(map[string]any{"Error": "InstallationLookupFailed"})
			return
		}
		if conn.Send(map[string]any{"Status": "Complete"}) == nil {
			documents.Serve(raw)
		}
	}
}
