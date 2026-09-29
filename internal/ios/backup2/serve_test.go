package backup2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/iostest"
)

// memStorage is a Storage over maps, keyed by device paths.
type memStorage struct {
	mu      sync.Mutex
	files   map[string][]byte
	dirs    map[string]bool
	aborted int
}

func newMemStorage() *memStorage {
	return &memStorage{files: map[string][]byte{}, dirs: map[string]bool{}}
}

func (m *memStorage) FreeSpace() uint64 { return 1 << 40 }

func (m *memStorage) Open(name string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type memWriter struct {
	m    *memStorage
	name string
	bytes.Buffer
}

func (w *memWriter) Commit() error {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	w.m.files[w.name] = w.Bytes()
	return nil
}

func (w *memWriter) Abort() {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	w.m.aborted++
}

func (m *memStorage) Create(name string) (FileWriter, error) {
	return &memWriter{m: m, name: name}, nil
}

func (m *memStorage) MakeDirAll(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[name] = true
	return nil
}

func (m *memStorage) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, name)
	delete(m.dirs, name)
	return nil
}

func (m *memStorage) Rename(from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[from]
	if !ok {
		return fs.ErrNotExist
	}
	delete(m.files, from)
	m.files[to] = data
	return nil
}

func (m *memStorage) Copy(from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[to] = m.files[from]
	return nil
}

func (m *memStorage) Exists(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.files[name]
	return ok || m.dirs[name]
}

func (m *memStorage) List(dir string) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var entries []Entry
	for name, data := range m.files {
		if path.Dir(name) == dir {
			entries = append(entries, Entry{Name: path.Base(name), Size: int64(len(data)), Modified: time.Unix(1_800_000_000, 0)})
		}
	}
	return entries, nil
}

// converse opens a host Conn to a scripted device.
func converse(t *testing.T, script func(dl *iostest.DeviceLink)) *Conn {
	t.Helper()
	host, device := net.Pipe()
	t.Cleanup(func() { _ = host.Close() })
	go func() {
		defer device.Close()
		iostest.Backup2(t, script)(device)
	}()
	conn, err := Open(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestBackupConversation(t *testing.T) {
	storage := newMemStorage()
	storage.files["UDID/Status.plist"] = []byte("old status")
	conn := converse(t, func(dl *iostest.DeviceLink) {
		request := dl.Request()
		if request["MessageName"] != "Backup" || request["TargetIdentifier"] != "UDID" || request["SourceIdentifier"] != "UDID" {
			t.Errorf("backup request = %v", request)
		}
		if code, free := dl.Ask("DLMessageGetFreeDiskSpace", "UDID"); code != 0 || free != uint64(1<<40) {
			t.Errorf("free space = %d %v", code, free)
		}
		received, code, failures := dl.Download(10, "UDID/Status.plist", "UDID/Manifest.plist")
		if !bytes.Equal(received["UDID/Status.plist"], []byte("old status")) || code != -13 {
			t.Errorf("download = %q, %d", received, code)
		}
		if failure, _ := failures.(map[string]any)["UDID/Manifest.plist"].(map[string]any); failure["DLFileErrorCode"] != int64(-6) && failure["DLFileErrorCode"] != uint64(0xFFFFFFFFFFFFFFFA) {
			t.Errorf("missing file failure = %v", failures)
		}
		big := bytes.Repeat([]byte("x"), 300<<10)
		if code := dl.Upload(60,
			iostest.UploadFile{Path: "UDID/Manifest.db", Data: big},
			iostest.UploadFile{Path: ".b/1/Status.plist", Data: []byte("new status"), Trailer: "done"},
		); code != 0 {
			t.Errorf("upload status = %d", code)
		}
		if code, _ := dl.Ask("DLMessageMoveFiles", map[string]any{".b/1/Status.plist": "UDID/Status.plist"}, map[string]any{}, 80.0); code != 0 {
			t.Errorf("move status = %d", code)
		}
		if code, _ := dl.Ask("DLMessageCopyItem", "UDID/Status.plist", "UDID/Status.copy"); code != 0 {
			t.Errorf("copy status = %d", code)
		}
		if code, _ := dl.Ask("DLMessageRemoveFiles", []any{"UDID/Status.copy", "UDID/absent"}, map[string]any{}, 90.0); code != 0 {
			t.Errorf("remove status = %d", code)
		}
		if code, listing := dl.Ask("DLContentsOfDirectory", "UDID"); code != 0 || len(listing.(map[string]any)) != 2 {
			t.Errorf("listing = %d %v", code, listing)
		}
		if code, _ := dl.Ask("DLMessagePurgeDiskSpace"); code != -1 {
			t.Errorf("unknown message status = %d", code)
		}
		dl.Finish(0, "")
	})
	if err := conn.Backup(t.Context(), "UDID", "UDID"); err != nil {
		t.Fatal(err)
	}
	var progress []Progress
	outcome, err := conn.Serve(t.Context(), storage, func(p Progress) { progress = append(progress, p) })
	if err != nil {
		t.Fatal(err)
	}
	if err := Verdict(outcome); err != nil {
		t.Errorf("verdict = %v", err)
	}
	if want := []string{"UDID/Manifest.db", "UDID/Status.plist"}; !reflect.DeepEqual(slices.Sorted(maps.Keys(storage.files)), want) {
		t.Errorf("files = %v, want %v", slices.Sorted(maps.Keys(storage.files)), want)
	}
	if string(storage.files["UDID/Status.plist"]) != "new status" {
		t.Errorf("a trailer-ended file was not kept: %q", storage.files["UDID/Status.plist"])
	}
	if len(progress) == 0 || progress[0] != (Progress{Percent: -1}) {
		t.Fatalf("progress = %v, want the start first", progress)
	}
	last := progress[len(progress)-1]
	for i := 1; i < len(progress); i++ {
		if progress[i].Percent < progress[i-1].Percent && progress[i].Percent >= 0 || progress[i].Bytes < progress[i-1].Bytes {
			t.Fatalf("progress went back: %v", progress)
		}
	}
	if last.Percent != 90 || last.Bytes != uint64(300<<10+len("old status")+len("new status")) {
		t.Errorf("last progress = %+v", last)
	}
}

func TestUploadCutShortKeepsNothing(t *testing.T) {
	storage := newMemStorage()
	conn := converse(t, func(dl *iostest.DeviceLink) {
		dl.Request()
		dl.Upload(50, iostest.UploadFile{Path: "UDID/Manifest.db", Data: []byte("partial"), Cut: true})
	})
	if err := conn.Backup(t.Context(), "UDID", "UDID"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Serve(t.Context(), storage, nil); err == nil {
		t.Fatal("a cut upload served without an error")
	}
	if len(storage.files) != 0 || storage.aborted != 1 {
		t.Errorf("files = %v, aborted = %d", storage.files, storage.aborted)
	}
}

// cancellingStorage cancels the conversation as the host starts on a file,
// and gives whatever heeds the cancel time to cut the file short.
type cancellingStorage struct {
	*memStorage
	cancel context.CancelFunc
}

func (c cancellingStorage) Open(name string) (io.ReadCloser, error) {
	c.cancel()
	time.Sleep(50 * time.Millisecond)
	return c.memStorage.Open(name)
}

// A cancel mid-message, as Finder honours one: the message is finished, the
// conversation stops at the next boundary and the host says goodbye.
func TestCancelStopsBetweenMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	storage := cancellingStorage{newMemStorage(), cancel}
	storage.files["UDID/Manifest.db"] = []byte("db")
	farewell := make(chan bool, 1)
	conn := converse(t, func(dl *iostest.DeviceLink) {
		dl.Request()
		if received, code, _ := dl.Download(50, "UDID/Manifest.db"); string(received["UDID/Manifest.db"]) != "db" || code != 0 {
			t.Errorf("the message in flight was cut: %q, %d", received, code)
		}
		farewell <- dl.Wait()
	})
	if err := conn.Restore(ctx, "UDID", "UDID", RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Serve(ctx, storage, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v, want cancelled", err)
	}
	_ = conn.Close()
	if !<-farewell {
		t.Error("the host left without DLMessageDisconnect")
	}
}

func TestVerdict(t *testing.T) {
	answer := func(fields ...any) *Dict {
		dict := &Dict{}
		for i := 0; i < len(fields); i += 2 {
			dict.Keys = append(dict.Keys, fields[i].(string))
			dict.Values = append(dict.Values, fields[i+1])
		}
		return dict
	}
	if err := Verdict(answer("ErrorCode", int64(0))); err != nil {
		t.Errorf("success = %v", err)
	}
	err := Verdict(answer("ErrorCode", int64(CodeDeviceLocked), "ErrorDescription", "locked"))
	if refusal, ok := errors.AsType[*Error](err); !ok || refusal.Code != CodeDeviceLocked || !strings.Contains(err.Error(), "locked") {
		t.Errorf("refusal = %v", err)
	}
	for _, outcome := range []*Dict{nil, answer("ErrorDescription", "no code"), answer("ErrorCode", "zero")} {
		if err := Verdict(outcome); !errors.Is(err, ios.ErrProtocol) {
			t.Errorf("Verdict(%v) = %v, want ErrProtocol", outcome, err)
		}
	}
}

// Without storage the host still answers each request, or the device would
// wait for the reply instead of giving its verdict.
func TestServeWithoutStorageRefuses(t *testing.T) {
	conn := converse(t, func(dl *iostest.DeviceLink) {
		request := dl.Request()
		if request["MessageName"] != "ChangePassword" || request["OldPassword"] != nil || request["NewPassword"] != "secret" {
			t.Errorf("request = %v", request)
		}
		if code, _ := dl.Ask("DLMessageGetFreeDiskSpace", "UDID"); code != -1 {
			t.Errorf("free space answered %d, want a refusal", code)
		}
		dl.Finish(CodeWrongPassword, "wrong password")
	})
	ctx := context.Background()
	if err := conn.ChangePassword(ctx, "UDID", "", "secret"); err != nil {
		t.Fatal(err)
	}
	outcome, err := conn.Serve(ctx, nil, nil)
	if refusal, ok := errors.AsType[*Error](Verdict(outcome)); err != nil || !ok || refusal.Code != CodeWrongPassword {
		t.Errorf("outcome = %v, %v", outcome, err)
	}
}
