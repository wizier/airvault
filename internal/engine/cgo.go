package engine

/*
// Stable link path: `make shim` (release) and `make shim-dev` (debug) each copy
// their archive here, so this one line works for both dev and Docker.
#cgo LDFLAGS: ${SRCDIR}/../../engine/rust/target/link/libairvault_shim.a
#cgo CFLAGS: -I${SRCDIR}/../../engine/rust/include
#cgo linux LDFLAGS: -lm -ldl -lpthread -lssl -lcrypto
#cgo darwin LDFLAGS: -framework CoreFoundation -framework Security

#include <stdlib.h>
#include "airvault.h"

// Backup/restore progress push: the shim calls back per payload chunk and per
// device progress frame. Trampolines are defined in cgo_watch.go.
void av_backup_trampoline(size_t callback_id, int32_t phase, double percent, uint64_t bytes);
void av_install_trampoline(size_t callback_id, int32_t phase, uint64_t percent);

*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"runtime/cgo"
	"sync"
	"time"
	"unsafe"

	airlog "github.com/wizier/airvault/internal/logging"
)

// Error kinds come from the generated Rust ABI header, keeping one numeric
// contract across the language boundary.
const (
	ErrorInvalidArgument       ErrorKind = ErrorKind(C.AV_ERROR_INVALID_ARGUMENT)
	ErrorDeviceUnavailable     ErrorKind = ErrorKind(C.AV_ERROR_DEVICE_UNAVAILABLE)
	ErrorDeviceLocked          ErrorKind = ErrorKind(C.AV_ERROR_DEVICE_LOCKED)
	ErrorTrustRequired         ErrorKind = ErrorKind(C.AV_ERROR_TRUST_REQUIRED)
	ErrorUserDenied            ErrorKind = ErrorKind(C.AV_ERROR_USER_DENIED)
	ErrorBusy                  ErrorKind = ErrorKind(C.AV_ERROR_BUSY)
	ErrorTimeout               ErrorKind = ErrorKind(C.AV_ERROR_TIMEOUT)
	ErrorCancelled             ErrorKind = ErrorKind(C.AV_ERROR_CANCELLED)
	ErrorProtocol              ErrorKind = ErrorKind(C.AV_ERROR_PROTOCOL)
	ErrorStorageFull           ErrorKind = ErrorKind(C.AV_ERROR_STORAGE_FULL)
	ErrorIntegrity             ErrorKind = ErrorKind(C.AV_ERROR_INTEGRITY)
	ErrorUnsupported           ErrorKind = ErrorKind(C.AV_ERROR_UNSUPPORTED)
	ErrorInternal              ErrorKind = ErrorKind(C.AV_ERROR_INTERNAL)
	ErrorInvalidBackupPassword ErrorKind = ErrorKind(C.AV_ERROR_INVALID_BACKUP_PASSWORD)
	ErrorOutcomeUnknown        ErrorKind = ErrorKind(C.AV_ERROR_OUTCOME_UNKNOWN)
	ErrorFindMyEnabled         ErrorKind = ErrorKind(C.AV_ERROR_FIND_MY_ENABLED)
)

// New returns an explicitly-owned cgo engine backed by the Rust idevice shim.
func New(config Config) (*Engine, error) {
	root, err := filepath.Abs(config.BackupRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve engine backup root: %w", err)
	}
	pairingRoot, err := filepath.Abs(config.PairingRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve engine pairing root: %w", err)
	}
	croot := C.CString(root)
	defer C.free(unsafe.Pointer(croot))
	cpairing := C.CString(pairingRoot)
	defer C.free(unsafe.Pointer(cpairing))
	cmux := C.CString(config.MuxAddress)
	defer C.free(unsafe.Pointer(cmux))
	var errStr *C.char
	native := C.av_engine_new(croot, cpairing, cmux, &errStr)
	if errStr != nil {
		defer C.av_string_free(errStr)
	}
	if native == nil {
		return nil, fmt.Errorf("create native engine: %s", cstr(errStr))
	}
	slog.Info("engine: cgo idevice shim active")
	return &Engine{native: native}, nil
}

type Engine struct {
	mu     sync.RWMutex
	native *C.AvEngine
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.native != nil {
		C.av_engine_free(e.native)
		e.native = nil
	}
	return nil
}

func (e *Engine) engine(ctx context.Context) (*C.AvEngine, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	e.mu.RLock()
	if e.native == nil {
		e.mu.RUnlock()
		return nil, nil, errors.New("engine is closed")
	}
	return e.native, e.mu.RUnlock, nil
}

func cstr(c *C.char) string {
	if c == nil {
		return ""
	}
	return C.GoString(c)
}

func cbool(b bool) C.int32_t {
	if b {
		return 1
	}
	return 0
}

// callEngineError is the single error parse at the FFI boundary: rc 0 =
// success, any other rc is an AV_ERROR_* kind whose Rust-owned detail is
// copied and freed here. Every request/response av_* call goes through it.
func callEngineError(call func(out *C.AvError) C.int32_t) error {
	var native C.AvError
	rc := call(&native)
	if native.detail.ptr != nil {
		defer C.av_buffer_free(native.detail)
	}
	if rc == 0 {
		return nil
	}
	detail := "device engine operation failed"
	if native.detail.ptr != nil && native.detail.len > 0 {
		detail = string(C.GoBytes(unsafe.Pointer(native.detail.ptr), C.int(native.detail.len)))
	}
	return &Error{Kind: ErrorKind(rc), Detail: detail}
}

// callJSON decodes one JSON-returning shim call's payload into dst; the error
// protocol itself lives in callEngineError.
func callJSON(dst any, call func(out **C.char, e *C.AvError) C.int32_t) error {
	var out *C.char
	err := callEngineError(func(e *C.AvError) C.int32_t { return call(&out, e) })
	if out != nil {
		defer C.av_string_free(out)
	}
	if err != nil {
		return err
	}
	if s := cstr(out); s != "" {
		return json.Unmarshal([]byte(s), dst)
	}
	return nil
}

// callString returns one string-returning shim call's payload.
func callString(call func(out **C.char, e *C.AvError) C.int32_t) (string, error) {
	var out *C.char
	err := callEngineError(func(e *C.AvError) C.int32_t { return call(&out, e) })
	if out != nil {
		defer C.av_string_free(out)
	}
	if err != nil {
		return "", err
	}
	return cstr(out), nil
}

// callBufferBytes copies and releases one Rust-owned binary response.
func callBufferBytes(call func(out *C.AvBuffer, e *C.AvError) C.int32_t) ([]byte, error) {
	var out C.AvBuffer
	err := callEngineError(func(e *C.AvError) C.int32_t { return call(&out, e) })
	if out.ptr != nil {
		defer C.av_buffer_free(out)
	}
	if err != nil {
		return nil, err
	}
	if out.len == 0 {
		return []byte{}, nil
	}
	return C.GoBytes(unsafe.Pointer(out.ptr), C.int(out.len)), nil
}

// The req* variants run one request/response call under the engine read-lock:
// acquire, call, release. Long transfers and stream constructors, which must
// hold the engine past the call, acquire manually instead.
func (e *Engine) req(ctx context.Context, call func(*C.AvEngine, *C.AvError) C.int32_t) error {
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return err
	}
	defer releaseEngine()
	return callEngineError(func(out *C.AvError) C.int32_t { return call(native, out) })
}

func (e *Engine) reqJSON(ctx context.Context, dst any, call func(*C.AvEngine, **C.char, *C.AvError) C.int32_t) error {
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return err
	}
	defer releaseEngine()
	return callJSON(dst, func(out **C.char, errOut *C.AvError) C.int32_t { return call(native, out, errOut) })
}

func (e *Engine) reqString(ctx context.Context, call func(*C.AvEngine, **C.char, *C.AvError) C.int32_t) (string, error) {
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return "", err
	}
	defer releaseEngine()
	return callString(func(out **C.char, errOut *C.AvError) C.int32_t { return call(native, out, errOut) })
}

func (e *Engine) reqBytes(ctx context.Context, call func(*C.AvEngine, *C.AvBuffer, *C.AvError) C.int32_t) ([]byte, error) {
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseEngine()
	return callBufferBytes(func(out *C.AvBuffer, errOut *C.AvError) C.int32_t { return call(native, out, errOut) })
}

// pullStream runs blocking pulls on an owned stream until a payload or a
// failure, retrying AV_STREAM_CONTINUE internally. call's outputs are written
// only on rc 0; errStr is owned and freed here.
func pullStream[H comparable](ctx context.Context, h *nativeHandle[H], call func(handle H, errStr **C.char) C.int32_t) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		handle, leave, ok := h.enter()
		if !ok {
			return io.ErrClosedPipe
		}
		var errStr *C.char
		rc := call(handle, &errStr)
		leave()
		detail := cstr(errStr)
		if errStr != nil {
			C.av_string_free(errStr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch rc {
		case 0:
			return nil
		case C.AV_STREAM_CONTINUE:
			continue
		case C.AV_STREAM_CLOSED:
			return io.ErrClosedPipe
		default:
			if detail == "" {
				detail = "device engine stream failed"
			}
			return errors.New(detail)
		}
	}
}

func (e *Engine) ProbeMux(ctx context.Context) (MuxState, error) {
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return MuxUnavailable, err
	}
	defer releaseEngine()
	if C.av_mux_probe(native) == 1 {
		return MuxAvailable, nil
	}
	return MuxUnavailable, nil
}

func (e *Engine) InspectDevices(ctx context.Context) ([]DeviceInfo, error) {
	var devices []DeviceInfo
	err := e.reqJSON(ctx, &devices, func(native *C.AvEngine, out **C.char, e *C.AvError) C.int32_t {
		return C.av_devices_inspect(native, out, e)
	})
	return devices, err
}

func (e *Engine) ListPresence(ctx context.Context) ([]DevicePresence, error) {
	var raw []rawPresence
	err := e.reqJSON(ctx, &raw, func(native *C.AvEngine, out **C.char, e *C.AvError) C.int32_t {
		return C.av_devices_list(native, out, e)
	})
	return presenceFromRaw(raw), err
}

func (e *Engine) Battery(ctx context.Context, device DeviceID) (Battery, error) {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	var battery Battery
	err := e.reqJSON(ctx, &battery, func(native *C.AvEngine, out **C.char, e *C.AvError) C.int32_t {
		return C.av_device_battery(native, cu, out, e)
	})
	return battery, err
}

func (e *Engine) ListUSBDevices(ctx context.Context) ([]USBDevice, error) {
	var devices []USBDevice
	err := e.reqJSON(ctx, &devices, func(native *C.AvEngine, out **C.char, e *C.AvError) C.int32_t {
		return C.av_usb_devices_list(native, out, e)
	})
	return devices, err
}

func (e *Engine) AdvancePairing(ctx context.Context, device DeviceID) (PairingOutcome, error) {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	status, err := e.reqString(ctx, func(native *C.AvEngine, out **C.char, e *C.AvError) C.int32_t {
		return C.av_pairing_advance(native, cu, cjob, out, e)
	})
	if err != nil {
		return TrustError, err
	}
	return PairingOutcome(status), nil
}

func (e *Engine) ChangeBackupPassword(ctx context.Context, device DeviceID, oldPassword, newPassword string) (BackupPasswordResult, error) {
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return BackupPasswordResult{}, err
	}
	defer releaseEngine()
	udid := string(device)
	cu := C.CString(udid)
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	cold := C.CString(oldPassword)
	defer C.free(unsafe.Pointer(cold))
	cnew := C.CString(newPassword)
	defer C.free(unsafe.Pointer(cnew))
	var encrypted C.int32_t = -1
	stopCancellation := watchOperationCancellation(ctx, native, airlog.JobID(ctx))
	err = callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_backup_password_change(native, cu, cjob, cold, cnew, &encrypted, e)
	})
	stopCancellation()
	result := BackupPasswordResult{EncryptionKnown: encrypted >= 0}
	if result.EncryptionKnown {
		result.Encrypted = encrypted != 0
	}
	return result, err
}

// Unpair asks the device to forget this host and clears the host-side pairing
// state. The device-side revoke is best effort and only logged by the shim; an
// error here means the host state itself survived.
func (e *Engine) Unpair(ctx context.Context, device DeviceID) error {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	return e.req(ctx, func(native *C.AvEngine, out *C.AvError) C.int32_t {
		return C.av_pairing_unpair(native, cu, out)
	})
}

// PresenceWatcher owns one pull-based mux listener until Close.
type PresenceWatcher struct {
	ctx    context.Context
	handle nativeHandle[*C.AvPresenceWatch]
}

func (e *Engine) OpenPresenceWatcher(ctx context.Context) (*PresenceWatcher, error) {
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseEngine()
	var handle *C.AvPresenceWatch
	if err := callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_device_watch_open(native, &handle, e)
	}); err != nil {
		return nil, err
	}
	watcher := &PresenceWatcher{ctx: ctx, handle: newNativeHandle(handle)}
	watcher.handle.closeOnContext(ctx, func() { _ = watcher.Close() })
	return watcher, nil
}

func (w *PresenceWatcher) Next() (PresenceState, error) {
	var out *C.char
	err := pullStream(w.ctx, &w.handle, func(handle *C.AvPresenceWatch, errStr **C.char) C.int32_t {
		return C.av_device_watch_next(handle, &out, errStr)
	})
	raw := cstr(out)
	if out != nil {
		C.av_string_free(out)
	}
	if err != nil {
		return PresenceState{}, err
	}
	var payload struct {
		Version int           `json:"version"`
		Up      bool          `json:"up"`
		Devices []rawPresence `json:"devices"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return PresenceState{}, fmt.Errorf("decode presence state: %w", err)
	}
	if payload.Version != 1 {
		return PresenceState{}, fmt.Errorf("decode presence state: unsupported version %d", payload.Version)
	}
	state := PresenceState{MuxState: MuxUnavailable}
	if payload.Up {
		state.MuxState = MuxAvailable
	}
	state.Devices = presenceFromRaw(payload.Devices)
	return state, nil
}

func (w *PresenceWatcher) Close() error {
	w.handle.close(
		func(handle *C.AvPresenceWatch) { C.av_device_watch_cancel(handle) },
		func(handle *C.AvPresenceWatch) { C.av_device_watch_close(handle) },
	)
	return nil
}

// watchOperationCancellation bridges context cancellation to the exact native
// operation. Registration happens inside the blocking Rust call, so an early
// cancellation retries until that registration exists or the call returns.
func watchOperationCancellation(ctx context.Context, native *C.AvEngine, operationID string) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			cop := C.CString(operationID)
			defer C.free(unsafe.Pointer(cop))
			for C.av_operation_cancel(native, cop) == C.AV_CANCEL_NOT_REGISTERED {
				select {
				case <-stop:
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

//export goBackupCallback
func goBackupCallback(callbackID C.size_t, phase C.int32_t, percent C.double, bytes C.uint64_t) {
	cgo.Handle(callbackID).Value().(*backupCallback).submit(int32(phase), float64(percent), uint64(bytes))
}

// BuildSnapshot blocks for the whole transfer — the documented exception to the
// shim-timeout rule; cancelling ctx aborts the exact device-link session. On
// success it returns the payload bytes the backup added to the object pool.
func (e *Engine) BuildSnapshot(ctx context.Context, req BuildSnapshotRequest, onProgress func(Progress)) (int64, error) {
	if req.OperationID == "" || req.DeviceID == "" || req.SnapshotID == "" {
		return 0, fmt.Errorf("backup operation, device, and snapshot id are required")
	}
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return 0, err
	}
	udid := string(req.DeviceID)
	cu := C.CString(udid)
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	coperation := C.CString(string(req.OperationID))
	defer C.free(unsafe.Pointer(coperation))
	csnapshot := C.CString(string(req.SnapshotID))
	defer C.free(unsafe.Pointer(csnapshot))
	cbase := C.CString(string(req.BaseSnapshotID))
	defer C.free(unsafe.Pointer(cbase))
	callback := newBackupCallback(onProgress)
	callbackID := cgo.NewHandle(callback)
	stopCancellation := watchOperationCancellation(ctx, native, string(req.OperationID))
	defer func() {
		stopCancellation()
		releaseEngine()
		callback.sink.close()
		callbackID.Delete()
	}()

	var added C.uint64_t
	err = callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_snapshot_build(native, cu, cjob, coperation, csnapshot, cbase,
			C.av_backup_cb(C.av_backup_trampoline), C.size_t(callbackID), &added, e)
	})
	if err != nil {
		return 0, err
	}
	return int64(added), nil
}

type cgoLockStream struct {
	ctx    context.Context
	handle nativeHandle[*C.AvLockStream]
}

func (e *Engine) OpenLockObserver(ctx context.Context, device DeviceID) (LockStream, error) {
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseEngine()
	cdevice := C.CString(string(device))
	defer C.free(unsafe.Pointer(cdevice))
	var handle *C.AvLockStream
	if err := callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_lock_observer_open(native, cdevice, &handle, e)
	}); err != nil {
		return nil, err
	}
	stream := &cgoLockStream{ctx: ctx, handle: newNativeHandle(handle)}
	stream.handle.closeOnContext(ctx, func() { _ = stream.Close() })
	return stream, nil
}

func (s *cgoLockStream) Next() (ScreenLockSignal, error) {
	var event C.int32_t
	err := pullStream(s.ctx, &s.handle, func(handle *C.AvLockStream, errStr **C.char) C.int32_t {
		return C.av_lock_observer_next(handle, &event, errStr)
	})
	if err != nil {
		return 0, err
	}
	switch event {
	case 1:
		return ScreenLockChanged, nil
	case 2:
		return ScreenLockComplete, nil
	default:
		return 0, &Error{Kind: ErrorProtocol, Detail: "invalid lock event"}
	}
}

func (s *cgoLockStream) Close() error {
	s.handle.close(
		func(handle *C.AvLockStream) { C.av_lock_observer_cancel(handle) },
		func(handle *C.AvLockStream) { C.av_lock_observer_close(handle) },
	)
	return nil
}

// RestoreSnapshot mirrors BuildSnapshot: a blocking transfer with call-local
// progress and exact cancellation.
func (e *Engine) RestoreSnapshot(ctx context.Context, req RestoreSnapshotRequest, onProgress func(Progress)) error {
	if req.OperationID == "" || req.TargetID == "" || req.Snapshot.SourceID == "" || req.Snapshot.SnapshotID == "" {
		return fmt.Errorf("restore operation, target, source, and snapshot id are required")
	}
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return err
	}
	udid := string(req.TargetID)
	cu := C.CString(udid)
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	coperation := C.CString(string(req.OperationID))
	defer C.free(unsafe.Pointer(coperation))
	csrc := C.CString(string(req.Snapshot.SourceID))
	defer C.free(unsafe.Pointer(csrc))
	csnapshot := C.CString(string(req.Snapshot.SnapshotID))
	defer C.free(unsafe.Pointer(csnapshot))
	cpw := C.CString(req.Password)
	defer C.free(unsafe.Pointer(cpw))
	callback := newBackupCallback(onProgress)
	callbackID := cgo.NewHandle(callback)
	stopCancellation := watchOperationCancellation(ctx, native, string(req.OperationID))
	defer func() {
		stopCancellation()
		releaseEngine()
		callback.sink.close()
		callbackID.Delete()
	}()

	return callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_snapshot_restore(native, cu, cjob, coperation, csrc, csnapshot, cpw, cbool(req.SystemFiles), cbool(req.Reboot),
			cbool(req.SettingsFromBackup), cbool(req.RemoveItemsNotRestored),
			C.av_backup_cb(C.av_backup_trampoline), C.size_t(callbackID), e)
	})
}

func (e *Engine) Power(ctx context.Context, device DeviceID, action PowerAction) error {
	var act C.int32_t
	switch action {
	case PowerRestart:
		act = 0
	case PowerShutdown:
		act = 1
	case PowerSleep:
		act = 2
	default:
		return fmt.Errorf("unknown power action %d", action)
	}
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	return e.req(ctx, func(native *C.AvEngine, out *C.AvError) C.int32_t {
		return C.av_device_power(native, cu, cjob, act, out)
	})
}

func (e *Engine) HardwareInfo(ctx context.Context, device DeviceID) (HardwareInfo, error) {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	var raw rawDeviceInfo
	err := e.reqJSON(ctx, &raw, func(native *C.AvEngine, out **C.char, e *C.AvError) C.int32_t {
		return C.av_device_hardware(native, cu, out, e)
	})
	if err != nil {
		return HardwareInfo{}, err
	}
	return raw.toHardwareInfo(), nil
}

// ActivationState reads the live activation state ("Activated", "Unactivated", ...).
func (e *Engine) ActivationState(ctx context.Context, device DeviceID) (string, error) {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	return e.reqString(ctx, func(native *C.AvEngine, out **C.char, e *C.AvError) C.int32_t {
		return C.av_activation_state(native, cu, cjob, out, e)
	})
}

// ActivationSessionInfo returns the session blob for Apple's drmHandshake POST
// (an XML plist).
func (e *Engine) ActivationSessionInfo(ctx context.Context, device DeviceID) ([]byte, error) {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	blob, err := e.reqString(ctx, func(native *C.AvEngine, out **C.char, e *C.AvError) C.int32_t {
		return C.av_activation_session_info(native, cu, cjob, out, e)
	})
	if err != nil {
		return nil, err
	}
	return []byte(blob), nil
}

// ActivationInfo builds the signed activation info (an XML plist) from Apple's
// raw drmHandshake response body.
func (e *Engine) ActivationInfo(ctx context.Context, device DeviceID, handshake []byte) ([]byte, error) {
	if len(handshake) == 0 {
		return nil, errors.New("empty drmHandshake response")
	}
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	h := (*C.uint8_t)(unsafe.Pointer(&handshake[0]))
	info, err := e.reqString(ctx, func(native *C.AvEngine, out **C.char, e *C.AvError) C.int32_t {
		return C.av_activation_info(native, cu, cjob, h, C.size_t(len(handshake)), out, e)
	})
	if err != nil {
		return nil, err
	}
	return []byte(info), nil
}

// ActivationFinish applies Apple's activation record on the phone; headers are
// the activation response headers (forwarded verbatim).
func (e *Engine) ActivationFinish(ctx context.Context, device DeviceID, record []byte, headers map[string]string) error {
	if len(record) == 0 {
		return errors.New("empty activation record")
	}
	headersJSON := ""
	if len(headers) > 0 {
		encoded, err := json.Marshal(headers)
		if err != nil {
			return err
		}
		headersJSON = string(encoded)
	}
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	r := (*C.uint8_t)(unsafe.Pointer(&record[0]))
	cheaders := C.CString(headersJSON)
	defer C.free(unsafe.Pointer(cheaders))
	return e.req(ctx, func(native *C.AvEngine, out *C.AvError) C.int32_t {
		return C.av_activation_finish(native, cu, cjob, r, C.size_t(len(record)), cheaders, out)
	})
}

// ListApps lists the device's user-installed applications (av_apps_list kind 0).
func (e *Engine) ListApps(ctx context.Context, device DeviceID) ([]App, error) {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	var apps []App
	err := e.reqJSON(ctx, &apps, func(native *C.AvEngine, out **C.char, e *C.AvError) C.int32_t {
		return C.av_apps_list(native, cu, 0, out, e)
	})
	return apps, err
}

func (e *Engine) AppIcon(ctx context.Context, device DeviceID, bundleID string) ([]byte, error) {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	cb := C.CString(bundleID)
	defer C.free(unsafe.Pointer(cb))
	return e.reqBytes(ctx, func(native *C.AvEngine, out *C.AvBuffer, e *C.AvError) C.int32_t {
		return C.av_app_icon(native, cu, cb, out, e)
	})
}

func (e *Engine) Wallpaper(ctx context.Context, device DeviceID, screen WallpaperScreen) ([]byte, error) {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	return e.reqBytes(ctx, func(native *C.AvEngine, out *C.AvBuffer, e *C.AvError) C.int32_t {
		return C.av_wallpaper_get(native, cu, cbool(screen == WallpaperLock), out, e)
	})
}

//export goInstallCallback
func goInstallCallback(callbackID C.size_t, phase C.int32_t, percent C.uint64_t) {
	progress := InstallProgress{Phase: InstallPhaseStaging, Percent: int(percent)}
	if phase == C.AV_INSTALL_PHASE_INSTALLING {
		progress.Phase = InstallPhaseInstalling
	}
	cgo.Handle(callbackID).Value().(*latestDispatcher[InstallProgress]).submit(progress)
}

// InstallApp uploads and installs a user-provided .ipa (a host file path),
// forwarding phase-local progress. Once native work starts it runs to a device
// result or the shim's hard timeout; request cancellation cannot undo install.
func (e *Engine) InstallApp(ctx context.Context, device DeviceID, ipaPath string, onProgress func(InstallProgress)) error {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	cp := C.CString(ipaPath)
	defer C.free(unsafe.Pointer(cp))
	callback := newLatestDispatcher("install progress", onProgress)
	callbackID := cgo.NewHandle(callback)
	defer func() {
		callback.close()
		callbackID.Delete()
	}()
	return e.req(ctx, func(native *C.AvEngine, out *C.AvError) C.int32_t {
		return C.av_app_install(native, cu, cjob, cp, C.av_install_cb(C.av_install_trampoline), C.size_t(callbackID), out)
	})
}

// UninstallApp removes an installed app by bundle id (installation_proxy).
func (e *Engine) UninstallApp(ctx context.Context, device DeviceID, bundleID string) error {
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	cjob := C.CString(airlog.JobID(ctx))
	defer C.free(unsafe.Pointer(cjob))
	cb := C.CString(bundleID)
	defer C.free(unsafe.Pointer(cb))
	return e.req(ctx, func(native *C.AvEngine, out *C.AvError) C.int32_t {
		return C.av_app_uninstall(native, cu, cjob, cb, out)
	})
}

func afcBytes(value string) (*C.uint8_t, C.size_t) {
	if value == "" {
		return nil, 0
	}
	ptr := C.CBytes([]byte(value))
	return (*C.uint8_t)(ptr), C.size_t(len(value))
}

type cgoAFCSession struct {
	ctx    context.Context
	handle nativeHandle[*C.AvAfcSession]
}

func newAFCSession(ctx context.Context, handle *C.AvAfcSession) *cgoAFCSession {
	session := &cgoAFCSession{ctx: ctx, handle: newNativeHandle(handle)}
	session.handle.closeOnContext(ctx, session.closeHandle)
	return session
}

func (s *cgoAFCSession) closeHandle() {
	s.handle.close(
		func(handle *C.AvAfcSession) { C.av_afc_cancel(handle) },
		func(handle *C.AvAfcSession) { C.av_afc_close(handle) },
	)
}

func (s *cgoAFCSession) closedError() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return io.ErrClosedPipe
}

func (s *cgoAFCSession) Close() error {
	s.closeHandle()
	return nil
}

func (s *cgoAFCSession) List(devicePath string) ([]string, error) {
	handle, leave, ok := s.handle.enter()
	if !ok {
		return nil, s.closedError()
	}
	defer leave()
	cp, cpLen := afcBytes(devicePath)
	if cp != nil {
		defer C.free(unsafe.Pointer(cp))
	}
	var names []string
	err := callJSON(&names, func(out **C.char, e *C.AvError) C.int32_t {
		return C.av_afc_list(handle, cp, cpLen, out, e)
	})
	if err := afcError(s.ctx, err); err != nil {
		return nil, err
	}
	return names, nil
}

func (s *cgoAFCSession) Stat(devicePath string) (AFCEntry, error) {
	handle, leave, ok := s.handle.enter()
	if !ok {
		return AFCEntry{}, s.closedError()
	}
	defer leave()
	cp, cpLen := afcBytes(devicePath)
	if cp != nil {
		defer C.free(unsafe.Pointer(cp))
	}
	var entry AFCEntry
	err := callJSON(&entry, func(out **C.char, e *C.AvError) C.int32_t {
		return C.av_afc_stat(handle, cp, cpLen, out, e)
	})
	if err := afcError(s.ctx, err); err != nil {
		return AFCEntry{}, err
	}
	return entry, nil
}

func (s *cgoAFCSession) Remove(devicePath string) error {
	handle, leave, ok := s.handle.enter()
	if !ok {
		return s.closedError()
	}
	defer leave()
	cp, cpLen := afcBytes(devicePath)
	if cp != nil {
		defer C.free(unsafe.Pointer(cp))
	}
	return afcError(s.ctx, callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_afc_remove(handle, cp, cpLen, e)
	}))
}

// afcReadBufPool recycles the 1 MiB read buffer so a thumbnail batch (many small
// ReadSmall calls on one session) does not allocate per file.
var afcReadBufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

func (s *cgoAFCSession) ReadSmall(devicePath string) ([]byte, error) {
	handle, leave, ok := s.handle.enter()
	if !ok {
		return nil, s.closedError()
	}
	defer leave()
	cp, cpLen := afcBytes(devicePath)
	if cp != nil {
		defer C.free(unsafe.Pointer(cp))
	}
	bufPtr := afcReadBufPool.Get().(*[]byte)
	defer afcReadBufPool.Put(bufPtr)
	buffer := *bufPtr
	var read C.size_t
	err := callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_afc_read_small(handle, cp, cpLen,
			(*C.uint8_t)(unsafe.Pointer(&buffer[0])), C.size_t(len(buffer)), &read, e)
	})
	if err := afcError(s.ctx, err); err != nil {
		return nil, err
	}
	out := make([]byte, int(read))
	copy(out, buffer[:int(read)])
	return out, nil
}

func (s *cgoAFCSession) Open(devicePath string) (AFCFile, error) {
	handle, leave, ok := s.handle.enter()
	if !ok {
		return nil, s.closedError()
	}
	defer leave()
	cp, cpLen := afcBytes(devicePath)
	if cp != nil {
		defer C.free(unsafe.Pointer(cp))
	}
	var size C.uint64_t
	var file *C.AvAfcFile
	openErr := afcError(s.ctx, callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_afc_file_open(handle, cp, cpLen, &size, &file, e)
	}))
	// Open consumes the session on every outcome. On success Rust has moved
	// the slot into `file`, so closing this wrapper only releases its shell.
	if !s.handle.detach(handle) {
		if file != nil {
			C.av_afc_file_close(file)
		}
		return nil, s.closedError()
	}
	s.handle.stopContextClose()
	C.av_afc_close(handle)
	if openErr != nil {
		return nil, openErr
	}
	if s.ctx.Err() != nil {
		C.av_afc_file_close(file)
		return nil, s.ctx.Err()
	}
	if uint64(size) > math.MaxInt64 {
		C.av_afc_file_close(file)
		return nil, errors.New("AFC file exceeds the supported size")
	}
	return newAFCFile(s.ctx, file, int64(size)), nil
}

type cgoAFCFile struct {
	ctx    context.Context
	handle nativeHandle[*C.AvAfcFile]
	size   int64
}

func newAFCFile(ctx context.Context, handle *C.AvAfcFile, size int64) *cgoAFCFile {
	file := &cgoAFCFile{ctx: ctx, handle: newNativeHandle(handle), size: size}
	file.handle.closeOnContext(ctx, func() { _ = file.Close() })
	return file
}

func (f *cgoAFCFile) Size() int64 { return f.size }

func (f *cgoAFCFile) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if len(buffer) > 1<<20 {
		buffer = buffer[:1<<20]
	}
	handle, leave, ok := f.handle.enter()
	if !ok {
		if f.ctx.Err() != nil {
			return 0, f.ctx.Err()
		}
		return 0, io.ErrClosedPipe
	}
	defer leave()
	var read C.size_t
	err := callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_afc_file_read(handle, (*C.uint8_t)(unsafe.Pointer(&buffer[0])), C.size_t(len(buffer)), &read, e)
	})
	if err := afcError(f.ctx, err); err != nil {
		return 0, err
	}
	if read == 0 {
		return 0, io.EOF
	}
	return int(read), nil
}

func (f *cgoAFCFile) Close() error {
	f.handle.close(
		func(handle *C.AvAfcFile) { C.av_afc_file_cancel(handle) },
		func(handle *C.AvAfcFile) { C.av_afc_file_close(handle) },
	)
	return nil
}

// OpenAFC opens one request-scoped session. A file produced by Open inherits
// the same context and remains independently closeable by its Go owner.
func (e *Engine) OpenAFC(ctx context.Context, device DeviceID, source AFCSource, bundleID string) (AFCSession, error) {
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseEngine()
	cu, cuLen := afcBytes(string(device))
	if cu != nil {
		defer C.free(unsafe.Pointer(cu))
	}
	cb, cbLen := afcBytes(bundleID)
	if cb != nil {
		defer C.free(unsafe.Pointer(cb))
	}
	var handle *C.AvAfcSession
	err = callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_afc_open(native, cu, cuLen, C.int32_t(source), cb, cbLen, &handle, e)
	})
	if err := afcError(ctx, err); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		C.av_afc_close(handle)
		return nil, ctx.Err()
	}
	return newAFCSession(ctx, handle), nil
}

// ConsoleStream owns one pull-based os_trace session until Close.
type ConsoleStream struct {
	ctx    context.Context
	handle nativeHandle[*C.AvConsoleStream]
}

func (e *Engine) OpenConsole(ctx context.Context, device DeviceID) (*ConsoleStream, error) {
	native, releaseEngine, err := e.engine(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseEngine()
	cu := C.CString(string(device))
	defer C.free(unsafe.Pointer(cu))
	var handle *C.AvConsoleStream
	if err := callEngineError(func(e *C.AvError) C.int32_t {
		return C.av_console_open(native, cu, &handle, e)
	}); err != nil {
		return nil, err
	}
	stream := &ConsoleStream{ctx: ctx, handle: newNativeHandle(handle)}
	stream.handle.closeOnContext(ctx, func() { _ = stream.Close() })
	return stream, nil
}

func (s *ConsoleStream) Next() (ConsoleLine, error) {
	var cjson *C.char
	err := pullStream(s.ctx, &s.handle, func(handle *C.AvConsoleStream, errStr **C.char) C.int32_t {
		return C.av_console_next(handle, &cjson, errStr)
	})
	raw := cstr(cjson)
	if cjson != nil {
		C.av_string_free(cjson)
	}
	if err != nil {
		return ConsoleLine{}, err
	}
	var line ConsoleLine
	if err := json.Unmarshal([]byte(raw), &line); err != nil {
		return ConsoleLine{}, fmt.Errorf("decode console record: %w", err)
	}
	return line, nil
}

func (s *ConsoleStream) Close() error {
	s.handle.close(
		func(handle *C.AvConsoleStream) { C.av_console_cancel(handle) },
		func(handle *C.AvConsoleStream) { C.av_console_close(handle) },
	)
	return nil
}
