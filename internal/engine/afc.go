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
// Open consumes the session because a file reader owns the transport until Close.
type AFCSession interface {
	List(path string) ([]string, error)
	Stat(path string) (AFCEntry, error)
	Open(path string) (AFCFile, error)
	// ReadSmall reads one whole small file without consuming the session, so one
	// session serves a batch of reads (thumbnails). Large files use Open.
	ReadSmall(path string) ([]byte, error)
	Remove(path string) error
	Close() error
}

// AFCFile is a cancellable reader that owns its AFC connection until Close.
type AFCFile interface {
	io.ReadCloser
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
	session := &afcSession{parent: ctx, pool: e.afc, origin: origin, client: client}
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
	parent context.Context // OpenAFC's; a file from Open inherits it
	ctx    context.Context
	cancel context.CancelCauseFunc
	pool   *afcPool
	origin afcOrigin

	mu     sync.Mutex // serializes operations with Close
	client *afc.Client
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

// Open consumes the session on every outcome; on failure the connection goes
// back to the pool.
func (s *afcSession) Open(path string) (AFCFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.client
	s.client = nil
	if client == nil {
		return nil, ended(s.ctx)
	}
	if err := checkPath(path); err != nil {
		s.pool.put(s.origin, client)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(s.ctx, deviceWorkTimeout)
	defer cancel()
	info, err := statFile(ctx, client, path)
	var file *afc.File
	if err == nil {
		file, err = client.Open(ctx, path, afc.ReadOnly)
	}
	if err != nil {
		s.pool.put(s.origin, client) // closes it unless idle
		return nil, s.failure(ctx, err)
	}
	f := &afcFile{pool: s.pool, origin: s.origin, client: client, file: file, size: info.Size, modTime: info.ModTime}
	f.ctx, f.cancel = context.WithCancelCause(s.parent)
	return f, nil
}

func (s *afcSession) Close() error {
	s.cancel(io.ErrClosedPipe)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		s.pool.put(s.origin, s.client)
		s.client = nil
	}
	return nil
}

func (s *afcSession) run(path string, op func(context.Context, *afc.Client) error) error {
	if err := checkPath(path); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return ended(s.ctx)
	}
	ctx, cancel := context.WithTimeout(s.ctx, deviceWorkTimeout)
	defer cancel()
	err := op(ctx, s.client)
	if !s.client.Idle() {
		_ = s.client.Close()
		s.client = nil
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
	ctx     context.Context
	cancel  context.CancelCauseFunc
	pool    *afcPool
	origin  afcOrigin
	size    int64
	modTime time.Time

	mu     sync.Mutex // serializes reads with Close
	client *afc.Client
	file   *afc.File
}

func (f *afcFile) Size() int64 { return f.size }

func (f *afcFile) ModTime() time.Time { return f.modTime }

func (f *afcFile) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n int
	err := f.run(func(ctx context.Context) (err error) {
		n, err = f.file.Read(ctx, p)
		return err
	})
	return n, err
}

func (f *afcFile) SeekTo(offset int64) error {
	return f.run(func(ctx context.Context) error { return f.file.Seek(ctx, offset) })
}

// Close pools the connection only when the device confirmed the close.
func (f *afcFile) Close() error {
	f.cancel(io.ErrClosedPipe)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), afcCloseTimeout)
	defer cancel()
	_ = f.file.Close(ctx)
	f.pool.put(f.origin, f.client) // closes it unless idle
	f.client, f.file = nil, nil
	return nil
}

func (f *afcFile) run(op func(context.Context) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return ended(f.ctx)
	}
	ctx, cancel := context.WithTimeout(f.ctx, deviceWorkTimeout)
	defer cancel()
	err := op(ctx)
	if f.client.Torn() {
		_ = f.client.Close()
		f.client, f.file = nil, nil
	}
	switch {
	case err == nil || err == io.EOF:
		return err
	case f.ctx.Err() != nil:
		return ended(f.ctx)
	}
	return failure(ctx, "device files", err)
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
