package backup2

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"time"

	"github.com/wizier/airvault/internal/ios"
)

// Storage is the host side of a transfer, addressed by device paths. Open's
// fs.ErrNotExist is normal: a first backup asks for state files it lacks.
type Storage interface {
	FreeSpace() uint64
	Open(path string) (io.ReadCloser, error)
	Create(path string) (FileWriter, error)
	MakeDirAll(path string) error
	Remove(path string) error
	Rename(from, to string) error
	Copy(from, to string) error
	Exists(path string) bool
	List(path string) ([]Entry, error)
}

type FileWriter interface {
	io.Writer
	Commit() error
	Abort()
}

type Entry struct {
	Name     string
	Dir      bool
	Size     int64
	Modified time.Time // zero when unknown
}

// Progress carries the device's percent, -1 until it gives one.
type Progress struct {
	Percent float64
	Bytes   uint64
}

const (
	codeSuccess     = 0x00
	codeErrorLocal  = 0x06
	codeErrorRemote = 0x0B
	codeFileData    = 0x0C
)

const (
	downloadChunk = 32 << 10 // what the reference hosts send
	maxBlock      = 64 << 20 // one upload data block
	emitEvery     = 256 << 10
)

// messageGrace is how long a message in flight may take to finish once ctx
// ends; past it the conversation is cut mid-message. Shutdown waits for it.
const messageGrace = 5 * time.Second

// Serve answers the device until it reports the outcome, nil when it
// disconnected without one. A file the host cannot give is reported to the
// device; only a broken conversation is an error. Like Finder, it heeds ctx
// only between messages. A nil storage refuses every request.
func (c *Conn) Serve(ctx context.Context, storage Storage, progress func(Progress)) (*Dict, error) {
	cutoff, cut := context.WithCancel(context.WithoutCancel(ctx))
	defer cut()
	defer context.AfterFunc(ctx, func() { time.AfterFunc(messageGrace, cut) })()
	if progress == nil {
		progress = func(Progress) {}
	}
	s := &server{conn: c, storage: storage, report: progress}
	for {
		var tag string
		var message []any
		// Only the wait for the device's next message ends with ctx.
		_, err := ios.Guard(ctx, c.conn, func() (err error) {
			tag, message, err = c.recv()
			return err
		})
		if err != nil {
			return nil, err
		}
		switch tag {
		case "DLMessageProcessMessage":
			outcome, _ := at(message, 1).(*Dict)
			return outcome, nil
		case "DLMessageDisconnect":
			return nil, nil
		}
		if _, err := ios.Guard(cutoff, c.conn, func() error { return s.handle(tag, message) }); err != nil {
			return nil, cmp.Or(ctx.Err(), err)
		}
	}
}

type server struct {
	conn    *Conn
	storage Storage
	report  func(Progress)
	tracker tracker
	started bool
}

func (s *server) handle(tag string, message []any) error {
	if s.storage == nil {
		return s.status(-1, "Operation not supported", nil)
	}
	if !s.started {
		s.started = true
		s.report(Progress{Percent: -1})
	}
	target := s.target(tag, message)
	var err error
	switch tag {
	case "DLMessageDownloadFiles":
		return s.download(message, target)
	case "DLMessageUploadFiles":
		return s.upload(message, target)
	case "DLMessageGetFreeDiskSpace":
		err = s.status(0, "", s.storage.FreeSpace())
	case "DLContentsOfDirectory":
		err = s.status(0, "", s.list(message))
	case "DLMessageCreateDirectory":
		err = s.status(s.makeDir(message), "", nil)
	case "DLMessageMoveFiles", "DLMessageMoveItems":
		err = s.status(s.move(message), "", map[string]any{})
	case "DLMessageRemoveFiles", "DLMessageRemoveItems":
		err = s.status(s.remove(message), "", map[string]any{})
	case "DLMessageCopyItem":
		err = s.status(s.copy(message), "", map[string]any{})
	default:
		err = s.status(-1, "Operation not supported", nil)
	}
	if err == nil && target > 0 {
		s.tracker.finishBatch(0, target)
		s.emit(0, batchPosition{}, 0)
	}
	return err
}

// target is the percent the device says this message's work ends at.
func (s *server) target(tag string, message []any) float64 {
	index := 3
	switch tag {
	case "DLMessageUploadFiles":
		index = 2
	case "DLMessageDownloadFiles", "DLMessageMoveFiles", "DLMessageMoveItems", "DLMessageRemoveFiles", "DLMessageRemoveItems":
	default:
		return 0
	}
	target, _ := at(message, index).(float64)
	return max(target, 0)
}

func (s *server) emit(batchBytes uint64, position batchPosition, target float64) {
	percent, bytes := s.tracker.snapshot(batchBytes, position, target)
	s.report(Progress{Percent: percent, Bytes: bytes})
}

func (s *server) status(code int64, text string, value any) error {
	if text == "" {
		text = emptyParameter
	}
	if value == nil {
		value = emptyParameter
	}
	return s.conn.send([]any{"DLMessageStatusResponse", code, text, value})
}

func (s *server) download(message []any, target float64) error {
	paths, _ := at(message, 1).([]any)
	failures := map[string]any{}
	var batch uint64
	for i, item := range paths {
		name, ok := item.(string)
		if !ok {
			continue
		}
		code, reason, err := s.sendFile(name, &batch, batchPosition{files: true, done: uint64(i), total: uint64(len(paths))}, target)
		if err != nil {
			return err
		}
		if code != 0 {
			failures[name] = map[string]any{"DLFileErrorString": reason, "DLFileErrorCode": code}
		}
		s.emit(batch, batchPosition{files: true, done: uint64(i + 1), total: uint64(len(paths))}, target)
	}
	if err := s.writeU32(0); err != nil {
		return err
	}
	s.tracker.finishBatch(batch, target)
	s.emit(0, batchPosition{}, 0)
	if len(failures) > 0 {
		return s.status(-13, "Multi status", failures)
	}
	return s.status(0, "", map[string]any{})
}

func (s *server) sendFile(name string, batch *uint64, position batchPosition, target float64) (int64, string, error) {
	if err := s.writeBlock(nil, name); err != nil {
		return 0, "", err
	}
	file, err := s.storage.Open(name)
	if err != nil {
		code := int64(-1)
		if errors.Is(err, fs.ErrNotExist) {
			code = -6
		}
		return code, err.Error(), s.writeBlock([]byte{codeErrorLocal}, err.Error())
	}
	defer file.Close()
	buffer := make([]byte, downloadChunk)
	for {
		n, err := file.Read(buffer)
		if n > 0 {
			if err := s.writeBlock([]byte{codeFileData}, string(buffer[:n])); err != nil {
				return 0, "", err
			}
			*batch += uint64(n)
			if s.tracker.shouldEmit(*batch) {
				s.emit(*batch, position, target)
			}
		}
		if err == io.EOF {
			return 0, "", s.writeBlock([]byte{codeSuccess}, "")
		}
		if err != nil {
			return -1, err.Error(), s.writeBlock([]byte{codeErrorLocal}, err.Error())
		}
	}
}

// writeBlock writes [u32 length][code][payload]; a nil code writes a path.
func (s *server) writeBlock(code []byte, payload string) error {
	if err := s.writeU32(uint32(len(code) + len(payload))); err != nil {
		return err
	}
	if _, err := s.conn.writer.Write(code); err != nil {
		return err
	}
	_, err := s.conn.writer.WriteString(payload)
	return err
}

func (s *server) writeU32(v uint32) error {
	return binary.Write(s.conn.writer, binary.BigEndian, v)
}

// upload keeps what arrived even when an error block ends a file: the
// device sends one as a routine trailer.
func (s *server) upload(message []any, target float64) error {
	total, _ := at(message, 3).(int64)
	var received uint64
	for {
		dirLen, err := s.readU32()
		if err != nil || dirLen == 0 {
			return errors.Join(err, s.finishUpload(received, target))
		}
		if _, err := s.read(dirLen); err != nil {
			return err
		}
		nameLen, err := s.readU32()
		if err != nil || nameLen == 0 {
			return errors.Join(err, s.finishUpload(received, target))
		}
		name, err := s.read(nameLen)
		if err != nil {
			return err
		}
		if err := s.receiveFile(string(name), &received, uint64(max(total, 0)), target); err != nil {
			return err
		}
	}
}

func (s *server) finishUpload(received uint64, target float64) error {
	s.tracker.finishBatch(received, target)
	s.emit(0, batchPosition{}, 0)
	return s.status(0, "", map[string]any{})
}

func (s *server) receiveFile(name string, received *uint64, total uint64, target float64) error {
	_ = s.storage.MakeDirAll(path.Dir(name))
	size, err := s.readU32()
	if err != nil || size == 0 {
		return err // no content: nothing is created
	}
	code, err := s.conn.reader.ReadByte()
	if err != nil {
		return err
	}
	_ = s.storage.Remove(name)
	file, err := s.storage.Create(name)
	if err != nil {
		return fmt.Errorf("receive %s: %w", name, err)
	}
	for code == codeFileData {
		data, err := s.read(size - 1)
		if err == nil {
			_, err = file.Write(data)
		}
		if err != nil {
			file.Abort()
			return fmt.Errorf("receive %s: %w", name, err)
		}
		*received += uint64(len(data))
		if s.tracker.shouldEmit(*received) {
			s.emit(*received, batchPosition{done: *received, total: total}, target)
		}
		if size, err = s.readU32(); err == nil && size > 0 {
			code, err = s.conn.reader.ReadByte()
		}
		if err != nil {
			file.Abort()
			return fmt.Errorf("receive %s: %w", name, err)
		}
		if size == 0 {
			break
		}
	}
	if size > 0 && code != codeSuccess && code != codeFileData {
		if _, err := s.read(size - 1); err != nil { // the device's error text
			file.Abort()
			return err
		}
	}
	if err := file.Commit(); err != nil {
		return fmt.Errorf("receive %s: %w", name, err)
	}
	return nil
}

func (s *server) readU32() (uint32, error) {
	var v uint32
	err := binary.Read(s.conn.reader, binary.BigEndian, &v)
	return v, err
}

func (s *server) read(n uint32) ([]byte, error) {
	if n > maxBlock {
		return nil, fmt.Errorf("%w: DeviceLink block of %d bytes", ios.ErrProtocol, n)
	}
	buffer := make([]byte, n)
	_, err := io.ReadFull(s.conn.reader, buffer)
	return buffer, err
}

func (s *server) list(message []any) map[string]any {
	listing := map[string]any{}
	dir, _ := at(message, 1).(string)
	entries, err := s.storage.List(dir)
	if err != nil {
		return listing
	}
	for _, entry := range entries {
		kind := "DLFileTypeRegular"
		if entry.Dir {
			kind = "DLFileTypeDirectory"
		}
		info := map[string]any{"DLFileType": kind, "DLFileSize": uint64(max(entry.Size, 0))}
		if !entry.Modified.IsZero() {
			info["DLFileModificationDate"] = entry.Modified
		}
		listing[entry.Name] = info
	}
	return listing
}

func (s *server) makeDir(message []any) int64 {
	dir, ok := at(message, 1).(string)
	if !ok || s.storage.MakeDirAll(dir) != nil {
		return -1
	}
	return 0
}

// move keeps the device's order and stops at the first failure.
func (s *server) move(message []any) int64 {
	moves, ok := at(message, 1).(*Dict)
	if !ok {
		return -1
	}
	for i, from := range moves.Keys {
		to, ok := moves.Values[i].(string)
		if !ok {
			continue
		}
		_ = s.storage.MakeDirAll(path.Dir(to))
		if s.storage.Rename(from, to) != nil {
			return -1
		}
	}
	return 0
}

func (s *server) remove(message []any) int64 {
	items, ok := at(message, 1).([]any)
	if !ok {
		return -1
	}
	for _, item := range items {
		if name, ok := item.(string); ok && s.storage.Exists(name) && s.storage.Remove(name) != nil {
			return -1
		}
	}
	return 0
}

func (s *server) copy(message []any) int64 {
	from, fromOK := at(message, 1).(string)
	to, toOK := at(message, 2).(string)
	if !fromOK || !toOK {
		return -1
	}
	_ = s.storage.MakeDirAll(path.Dir(to))
	if s.storage.Copy(from, to) != nil {
		return -1
	}
	return 0
}
