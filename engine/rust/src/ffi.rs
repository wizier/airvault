//! The shared C-ABI pieces: error and buffer types, string marshalling, panic
//! containment and the process-owned engine lifecycle. Operation adapters live
//! beside their protocol implementation; safe inner code receives
//! `EngineContext` and never retains caller-owned C pointers.

use std::ffi::{c_char, CStr, CString};
use std::future::Future;
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use crate::bounded;
use crate::engine_error::{EngineFailure, ErrorKind};
use crate::operation_registry::OperationRegistry;
use crate::provider::{block, EngineContext};
use crate::timeouts;

#[repr(C)]
pub struct AvBuffer {
    pub ptr: *mut u8,
    pub len: usize,
}

#[repr(C)]
pub struct AvError {
    pub detail: AvBuffer,
}

// One typed error contract for every request/response export: rc 0 = success,
// any other rc is an AV_ERROR_* kind and the AvError out-param carries detail.
pub const AV_ERROR_INVALID_ARGUMENT: i32 = 1;
pub const AV_ERROR_DEVICE_UNAVAILABLE: i32 = 2;
pub const AV_ERROR_DEVICE_LOCKED: i32 = 3;
pub const AV_ERROR_TRUST_REQUIRED: i32 = 4;
pub const AV_ERROR_USER_DENIED: i32 = 5;
pub const AV_ERROR_BUSY: i32 = 6;
pub const AV_ERROR_TIMEOUT: i32 = 7;
pub const AV_ERROR_CANCELLED: i32 = 8;
pub const AV_ERROR_PROTOCOL: i32 = 9;
pub const AV_ERROR_STORAGE_FULL: i32 = 10;
pub const AV_ERROR_INTEGRITY: i32 = 11;
pub const AV_ERROR_UNSUPPORTED: i32 = 12;
pub const AV_ERROR_INTERNAL: i32 = 13;
pub const AV_ERROR_INVALID_BACKUP_PASSWORD: i32 = 14;
/// The device may have accepted an irreversible mutation, but neither its
/// protocol verdict nor a post-condition probe can prove the final outcome.
pub const AV_ERROR_OUTCOME_UNKNOWN: i32 = 15;
/// The phone refuses backup restores while Find My iPhone is on
/// (MBErrorDomain 211).
pub const AV_ERROR_FIND_MY_ENABLED: i32 = 16;
/// The phone ended a backup without its owner confirming it: the passcode
/// prompt iOS raises before every host backup was dismissed or timed out
/// (MBErrorDomain 208 in a backup verdict).
pub const AV_ERROR_BACKUP_NOT_CONFIRMED: i32 = 17;

// Pull-stream rc microprotocol for av_*_next: CONTINUE = quiet window (call
// again), CLOSED = stream over; any other non-zero rc is a stream error with
// `err` set. Mirrored by pullStream in internal/engine/cgo.go. Kept apart from
// the AV_ERROR_* values so a typed stream error never reads as a status.
pub const AV_STREAM_CONTINUE: i32 = 100;
pub const AV_STREAM_CLOSED: i32 = 101;

/// av_operation_cancel: no operation is registered under this id yet, so the
/// caller may retry until registration lands or the operation returns.
pub const AV_CANCEL_NOT_REGISTERED: i32 = 3;

/// Convert UTF-8 into an owned C string without discarding diagnostics that
/// contain an interior NUL. C cannot represent that byte, so expose it as the
/// conventional visible `\\0` escape instead.
pub(crate) fn ffi_cstring(s: &str) -> CString {
    CString::new(s.replace('\0', "\\0")).expect("interior NULs are escaped")
}

/// Write an owned C string into an out-param (caller frees via av_string_free).
pub(crate) fn out_str(dst: *mut *mut c_char, s: &str) {
    if !dst.is_null() {
        let c = ffi_cstring(s);
        unsafe { *dst = c.into_raw() };
    }
}

/// Zeroes a required out-param before the call can fail; null is an invalid
/// argument. Only for plain outputs (pointers, integers, AvBuffer), where all
/// zero bytes is the empty value.
pub(crate) fn reset_out<T>(out: *mut T, what: &str) -> Result<(), EngineFailure> {
    if out.is_null() {
        return Err(EngineFailure::invalid_argument(what));
    }
    unsafe { out.write_bytes(0, 1) };
    Ok(())
}

/// Free any string handed out by this shim (including error text).
///
/// # Safety
/// `s` must be a string returned by this shim, or null.
#[no_mangle]
pub unsafe extern "C" fn av_string_free(s: *mut c_char) {
    if !s.is_null() {
        unsafe { drop(CString::from_raw(s)) };
    }
}

/// Catch panics so they never unwind across the C ABI.
pub(crate) fn guard<F: FnOnce() -> i32>(err: *mut *mut c_char, f: F) -> i32 {
    match catch_unwind(AssertUnwindSafe(f)) {
        Ok(rc) => rc,
        Err(_) => {
            // Free any detail the callee wrote before panicking.
            if !err.is_null() {
                unsafe { av_string_free(*err) };
                unsafe { *err = std::ptr::null_mut() };
            }
            out_str(err, "panic in airvault_shim");
            -1
        }
    }
}

/// The one path from a failure to a string-error export: detail into `err`,
/// kind as the rc.
pub(crate) fn write_failure(err: *mut *mut c_char, failure: EngineFailure) -> i32 {
    out_str(err, &failure.detail);
    failure.kind.code()
}

/// Runs one request/response export: rc 0 on success, otherwise the failure's
/// kind with its detail in `error`. A panic is contained and returns rc -1.
pub(crate) fn guard_error(
    error: *mut AvError,
    f: impl FnOnce() -> Result<(), EngineFailure>,
) -> i32 {
    let (rc, detail) = match catch_unwind(AssertUnwindSafe(f)) {
        Ok(Ok(())) => return 0,
        Ok(Err(failure)) => (failure.kind.code(), failure.detail),
        Err(_) => (-1, "panic in airvault_shim".to_owned()),
    };
    if !error.is_null() {
        unsafe { out_buffer(&mut (*error).detail, detail.into_bytes()) };
    }
    rc
}

/// Write owned bytes into an out-param (caller frees via av_buffer_free).
pub(crate) fn out_buffer(dst: *mut AvBuffer, bytes: Vec<u8>) {
    if dst.is_null() {
        return;
    }
    let mut bytes = bytes.into_boxed_slice();
    let buffer = AvBuffer {
        ptr: bytes.as_mut_ptr(),
        len: bytes.len(),
    };
    std::mem::forget(bytes);
    unsafe { *dst = buffer };
}

#[no_mangle]
pub unsafe extern "C" fn av_buffer_free(buffer: AvBuffer) {
    if !buffer.ptr.is_null() {
        let slice = std::ptr::slice_from_raw_parts_mut(buffer.ptr, buffer.len);
        unsafe { drop(Box::from_raw(slice)) };
    }
}

/// JSON-encode a response payload. The exported caller's guard contains the
/// impossible serializer panic before it can cross the C ABI.
pub(crate) fn to_json<T: serde::Serialize>(v: &T) -> String {
    serde_json::to_string(v).expect("shim payloads always serialize")
}

/// Blocks on `producer` under `limit`; an elapsed limit fails as
/// AV_ERROR_TIMEOUT with `timeout_msg`.
pub(crate) fn block_bounded<T, E: Into<EngineFailure>>(
    limit: Duration,
    timeout_msg: &str,
    producer: impl Future<Output = Result<T, E>>,
) -> Result<T, EngineFailure> {
    block(async {
        tokio::time::timeout(limit, producer)
            .await
            .map_err(|_| EngineFailure::new(ErrorKind::Timeout, timeout_msg))?
            .map_err(Into::into)
    })
}

/// Read a C string in-param.
pub(crate) unsafe fn in_str<'a>(p: *const c_char) -> Option<&'a str> {
    if p.is_null() {
        return None;
    }
    CStr::from_ptr(p).to_str().ok()
}

/// Read an optional C-string in-param as an owned String (absent/null → empty).
/// The idiom for job ids and passwords, which are optional at the boundary.
pub(crate) unsafe fn opt_owned(p: *const c_char) -> String {
    unsafe { in_str(p) }.unwrap_or("").to_owned()
}

/// Read a required (non-empty) C-string argument; `what` is the
/// AV_ERROR_INVALID_ARGUMENT detail otherwise.
pub(crate) unsafe fn req_str(p: *const c_char, what: &str) -> Result<String, EngineFailure> {
    match in_str(p) {
        Some(s) if !s.is_empty() => Ok(s.to_owned()),
        _ => Err(EngineFailure::invalid_argument(what)),
    }
}

/// Immutable native-engine configuration owned by Go through an opaque pointer.
pub struct AvEngine {
    context: Arc<EngineContext>,
    pub(crate) operations: OperationRegistry,
}

impl AvEngine {
    pub(crate) fn new(context: EngineContext) -> Self {
        Self {
            context: Arc::new(context),
            operations: OperationRegistry::default(),
        }
    }

    pub(crate) fn context(&self) -> &EngineContext {
        &self.context
    }

    pub(crate) fn context_arc(&self) -> Arc<EngineContext> {
        self.context.clone()
    }
}

/// The roots arrive absolute: engine.New resolves them with filepath.Abs.
unsafe fn engine_context(
    backup_root: *const c_char,
    pairing_root: *const c_char,
    mux_address: *const c_char,
) -> Result<EngineContext, EngineFailure> {
    let root = PathBuf::from(unsafe { req_str(backup_root, "bad backup root") }?);
    let pairing_root = PathBuf::from(unsafe { req_str(pairing_root, "bad pairing root") }?);
    EngineContext::new(root, pairing_root, unsafe { in_str(mux_address) })
        .map_err(EngineFailure::invalid_argument)
}

/// Creates an engine with immutable provider and storage configuration.
#[no_mangle]
pub extern "C" fn av_engine_new(
    backup_root: *const c_char,
    pairing_root: *const c_char,
    mux_address: *const c_char,
    err: *mut *mut c_char,
) -> *mut AvEngine {
    let mut engine = std::ptr::null_mut();
    guard(err, || {
        match unsafe { engine_context(backup_root, pairing_root, mux_address) } {
            Ok(context) => {
                engine = Box::into_raw(Box::new(AvEngine::new(context)));
                0
            }
            Err(failure) => write_failure(err, failure),
        }
    });
    engine
}

/// Releases an engine after every operation and stream has joined.
#[no_mangle]
pub unsafe extern "C" fn av_engine_free(engine: *mut AvEngine) {
    if !engine.is_null() {
        unsafe { drop(Box::from_raw(engine)) };
    }
}

/// Cancels the exact in-flight transfer or bounded device command. 0 = delivered,
/// AV_CANCEL_NOT_REGISTERED = operation is not running, other = bad input.
#[no_mangle]
pub extern "C" fn av_operation_cancel(engine: *mut AvEngine, operation_id: *const c_char) -> i32 {
    guard(std::ptr::null_mut(), || {
        let (Ok(engine), Ok(operation_id)) = (unsafe {
            (
                engine_ref(engine),
                req_str(operation_id, "bad operation id"),
            )
        }) else {
            return ErrorKind::InvalidArgument.code();
        };
        if engine.operations.cancel(&operation_id) {
            0
        } else {
            AV_CANCEL_NOT_REGISTERED
        }
    })
}

pub(crate) unsafe fn engine_ref<'a>(engine: *mut AvEngine) -> Result<&'a AvEngine, EngineFailure> {
    unsafe { engine.as_ref() }.ok_or_else(|| EngineFailure::invalid_argument("engine is closed"))
}

/// Unpacks the (engine, udid) pair every device export starts with.
pub(crate) unsafe fn engine_udid<'a>(
    engine: *mut AvEngine,
    udid: *const c_char,
) -> Result<(&'a AvEngine, String), EngineFailure> {
    Ok((unsafe { engine_ref(engine) }?, unsafe {
        req_str(udid, "bad udid")
    }?))
}

/// Reports whether the muxer (usbmuxd/netmuxd) is reachable. The ABI's one
/// predicate-style return: 1 = up, 0 = down (a hung socket counts as down).
#[no_mangle]
pub extern "C" fn av_mux_probe(engine: *mut AvEngine) -> i32 {
    let Ok(engine) = (unsafe { engine_ref(engine) }) else {
        return 0;
    };
    guard(std::ptr::null_mut(), || {
        let mux = engine.context().mux_addr();
        block(bounded::within(timeouts::MUX, mux.connect(0))).is_ok() as i32
    })
}
