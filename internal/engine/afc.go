package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
)

// AFCEntry is raw metadata for one path in an AFC file tree.
type AFCEntry struct {
	IsDir    bool
	Size     int64
	Modified int64 // unix seconds, 0 if unknown
}

// AFCSource selects which device service opens an AFC connection.
type AFCSource int32

const (
	AFCMedia        AFCSource = iota // whole media partition (com.apple.afc)
	AFCAppDocuments                  // one app's Documents container (house_arrest)
)

// AFCSession is one sequential conversation on an AFC connection; the engine
// pools idle connections, so Close returns a healthy one for the next session.
type AFCSession interface {
	List(path string) ([]string, error)
	Stat(path string) (AFCEntry, error)
	// Open streams a large file: it reads on the session, whose Close closes it.
	Open(path string) (AFCFile, error)
	// ReadSmall reads one whole small file, so one session serves a batch of
	// reads (thumbnails).
	ReadSmall(path string) ([]byte, error)
	Remove(path string) error
	Close() error
}

// AFCFile reads a file its session opened, until the session closes.
type AFCFile interface {
	io.Reader
	Size() int64
	ModTime() time.Time // zero if unknown
	// SeekTo moves the device read cursor to an absolute offset (one round trip).
	SeekTo(offset int64) error
}

const (
	maxSmallRead    = 1 << 20 // ReadSmall's bound: thumbnails, not files
	afcCloseTimeout = 2 * time.Second
	maxBundleID     = 512
	maxDevicePath   = 4096
)

func (e *Engine) OpenAFC(ctx context.Context, device DeviceID, source AFCSource, bundleID string) (AFCSession, error) {
	if (source == AFCMedia) != (bundleID == "") || source > AFCAppDocuments || len(bundleID) > maxBundleID {
		return nil, &Error{Kind: ErrorInvalidArgument, Detail: "bad AFC source"}
	}
	key := afcKey{udid: string(device), source: source, bundleID: bundleID}
	client, origin, err := e.checkoutAFC(ctx, key)
	if err != nil {
		return nil, err
	}
	session := &afcSession{pool: e.afc, origin: origin, client: client}
	session.ctx, session.cancel = context.WithCancelCause(ctx)
	return session, nil
}

func (e *Engine) checkoutAFC(ctx context.Context, key afcKey) (*afc.Client, afcOrigin, error) {
	var origin afcOrigin
	client, err := call(ctx, deviceWorkTimeout, "open device files", func(ctx context.Context) (*afc.Client, error) {
		device, err := e.device(ctx, key.udid)
		if errors.Is(err, errDeviceNotFound) {
			e.afc.forget(key.udid)
		}
		if err != nil {
			return nil, err
		}
		transport := afcTransport{connection: device.Connection, addr: device.Addr}
		if idle, ok := e.afc.take(key, transport); ok {
			if e.afc.now().Sub(idle.returned) < afcValidateAfter || validAFC(ctx, idle.client) {
				origin = idle.origin
				return idle.client, nil
			}
			_ = idle.client.Close()
		}
		origin = afcOrigin{key: key, transport: transport, created: e.afc.now()}
		return e.dialAFC(ctx, key)
	})
	return client, origin, err
}

func validAFC(ctx context.Context, client *afc.Client) bool {
	ctx, cancel := context.WithTimeout(ctx, afcValidateTimeout)
	defer cancel()
	_, err := client.Stat(ctx, "/")
	return err == nil
}

func (e *Engine) dialAFC(ctx context.Context, key afcKey) (*afc.Client, error) {
	if key.source == AFCMedia {
		conn, err := e.openService(ctx, key.udid, afc.Service)
		if err != nil {
			return nil, err
		}
		return afc.New(conn), nil
	}
	conn, err := e.openService(ctx, key.udid, ios.HouseArrestService)
	if err != nil {
		return nil, err
	}
	if err := ios.VendDocuments(ctx, conn, key.bundleID); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return afc.New(conn), nil
}

// afcSession: Close interrupts an operation in flight, and a torn connection
// is dropped, never pooled; the session then reports io.ErrClosedPipe.
type afcSession struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	pool   *afcPool
	origin afcOrigin

	mu     sync.Mutex // serializes operations with Close
	client *afc.Client
	file   *afc.File // what Open left open, for Close to close
}

func (s *afcSession) List(path string) ([]string, error) {
	var names []string
	err := s.run(path, func(ctx context.Context, client *afc.Client) (err error) {
		names, err = client.List(ctx, path)
		return err
	})
	return names, err
}

func (s *afcSession) Stat(path string) (AFCEntry, error) {
	var entry AFCEntry
	err := s.run(path, func(ctx context.Context, client *afc.Client) error {
		info, err := client.Stat(ctx, path)
		entry = afcEntry(info)
		return err
	})
	return entry, err
}

func (s *afcSession) Remove(path string) error {
	return s.run(path, func(ctx context.Context, client *afc.Client) error {
		return client.Remove(ctx, path)
	})
}

// ReadSmall keeps the session: the bulk path for thumbnails.
func (s *afcSession) ReadSmall(path string) ([]byte, error) {
	var data []byte
	err := s.run(path, func(ctx context.Context, client *afc.Client) error {
		info, err := statFile(ctx, client, path)
		if err != nil {
			return err
		}
		if info.Size > maxSmallRead {
			return &Error{Kind: ErrorInternal, Detail: fmt.Sprintf("%s is larger than the %d-byte read buffer", path, maxSmallRead)}
		}
		file, err := client.Open(ctx, path, afc.ReadOnly)
		if err != nil {
			return err
		}
		data, err = readAll(ctx, file, info.Size)
		// A descriptor left open keeps the connection out of the pool.
		return errors.Join(err, file.Close(ctx))
	})
	return data, err
}

func (s *afcSession) Open(path string) (AFCFile, error) {
	var opened *afcFile
	err := s.run(path, func(ctx context.Context, client *afc.Client) error {
		info, err := statFile(ctx, client, path)
		if err != nil {
			return err
		}
		file, err := client.Open(ctx, path, afc.ReadOnly)
		if err != nil {
			return err
		}
		s.file = file
		opened = &afcFile{session: s, file: file, size: info.Size, modTime: info.ModTime}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return opened, nil
}

// Close pools the connection only when the device confirmed the close of the
// file Open left open.
func (s *afcSession) Close() error {
	s.cancel(io.ErrClosedPipe)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil
	}
	if s.file != nil {
		ctx, cancel := context.WithTimeout(context.Background(), afcCloseTimeout)
		_ = s.file.Close(ctx)
		cancel()
	}
	s.pool.put(s.origin, s.client) // closes it unless idle
	s.client, s.file = nil, nil
	return nil
}

func (s *afcSession) run(path string, op func(context.Context, *afc.Client) error) error {
	if err := checkPath(path); err != nil {
		return err
	}
	return s.do(func(ctx context.Context) error { return op(ctx, s.client) })
}

// do runs one operation at a time; a torn connection ends the session.
func (s *afcSession) do(op func(context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return ended(s.ctx)
	}
	ctx, cancel := context.WithTimeout(s.ctx, deviceWorkTimeout)
	defer cancel()
	err := op(ctx)
	if s.client.Torn() {
		_ = s.client.Close()
		s.client, s.file = nil, nil
	}
	if err == io.EOF {
		return err
	}
	return s.failure(ctx, err)
}

// failure reports what interrupted an operation rather than the I/O error.
func (s *afcSession) failure(ctx context.Context, err error) error {
	if err != nil && s.ctx.Err() != nil {
		return ended(s.ctx)
	}
	return failure(ctx, "device files", err)
}

// ended is the Open context's error once it ended, otherwise io.ErrClosedPipe.
func ended(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return io.ErrClosedPipe
}

type afcFile struct {
	session *afcSession
	file    *afc.File
	size    int64
	modTime time.Time
}

func (f *afcFile) Size() int64 { return f.size }

func (f *afcFile) ModTime() time.Time { return f.modTime }

func (f *afcFile) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n int
	err := f.session.do(func(ctx context.Context) (err error) {
		n, err = f.file.Read(ctx, p)
		return err
	})
	return n, err
}

func (f *afcFile) SeekTo(offset int64) error {
	return f.session.do(func(ctx context.Context) error { return f.file.Seek(ctx, offset) })
}

func statFile(ctx context.Context, client *afc.Client, path string) (afc.FileInfo, error) {
	info, err := client.Stat(ctx, path)
	if err == nil && info.IsDir {
		err = afc.ErrObjectIsDir
	}
	return info, err
}

func readAll(ctx context.Context, file *afc.File, size int64) ([]byte, error) {
	data := make([]byte, 0, size)
	buffer := make([]byte, min(max(size, 1), afc.MaxTransfer))
	for int64(len(data)) < size {
		n, err := file.Read(ctx, buffer)
		data = append(data, buffer[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return data, nil
}

func afcEntry(info afc.FileInfo) AFCEntry {
	entry := AFCEntry{IsDir: info.IsDir, Size: info.Size}
	if !info.ModTime.IsZero() {
		entry.Modified = info.ModTime.Unix()
	}
	return entry
}

func checkPath(path string) error {
	if !strings.HasPrefix(path, "/") || len(path) > maxDevicePath || strings.ContainsRune(path, 0) {
		return &Error{Kind: ErrorInvalidArgument, Detail: fmt.Sprintf("bad device path %q", path)}
	}
	return nil
}
