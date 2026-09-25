//! Shared C-ABI types and the process-owned engine lifecycle.
//! Operation adapters live beside their protocol implementation; safe inner
//! code receives `EngineContext` and never retains caller-owned C pointers.

use std::ffi::{c_char, CString};
use std::path::PathBuf;
use std::sync::Arc;

use crate::engine_error::ErrorKind;
use crate::operation_registry::OperationRegistry;
use crate::{guard, opt_owned, out_str, req_str, EngineContext};

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

// Pull-stream rc microprotocol for av_*_next: CONTINUE = quiet window (call
// again), CLOSED = stream over; any other non-zero rc is a stream error with
// `err` set. Mirrored by pullStream in internal/engine/cgo.go. Kept apart from
// the AV_ERROR_* values so a typed stream error never reads as a status.
pub const AV_STREAM_CONTINUE: i32 = 100;
pub const AV_STREAM_CLOSED: i32 = 101;

/// av_operation_cancel: no operation is registered under this id yet, so the
/// caller may retry until registration lands or the operation returns.
pub const AV_CANCEL_NOT_REGISTERED: i32 = 3;

pub(crate) fn guard_error(error: *mut AvError, f: impl FnOnce(*mut *mut c_char) -> i32) -> i32 {
    let mut detail: *mut c_char = std::ptr::null_mut();
    let detail_out = &mut detail as *mut *mut c_char;
    let rc = guard(detail_out, || f(detail_out));
    if rc != 0 {
        let message = if detail.is_null() {
            "device engine operation failed".to_owned()
        } else {
            let value = unsafe { CString::from_raw(detail) };
            value.to_string_lossy().into_owned()
        };
        if !error.is_null() {
            unsafe {
                (*error).detail = AvBuffer {
                    ptr: std::ptr::null_mut(),
                    len: 0,
                };
            }
            let bytes = message.into_bytes();
            unsafe { out_buffer(&mut (*error).detail, bytes) };
        }
    }
    rc
}

pub(crate) fn out_buffer(dst: *mut AvBuffer, bytes: Vec<u8>) -> bool {
    if dst.is_null() {
        return false;
    }
    let mut bytes = bytes.into_boxed_slice();
    let buffer = AvBuffer {
        ptr: bytes.as_mut_ptr(),
        len: bytes.len(),
    };
    std::mem::forget(bytes);
    unsafe { *dst = buffer };
    true
}

#[no_mangle]
pub unsafe extern "C" fn av_buffer_free(buffer: AvBuffer) {
    if !buffer.ptr.is_null() {
        let slice = std::ptr::slice_from_raw_parts_mut(buffer.ptr, buffer.len);
        unsafe { drop(Box::from_raw(slice)) };
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

/// Required absolute-path argument for engine construction.
unsafe fn abs_root(p: *const c_char, err: *mut *mut c_char, what: &str) -> Option<PathBuf> {
    let value = unsafe { req_str(p, err, &format!("bad {what}")) }?;
    let path = PathBuf::from(value);
    if !path.is_absolute() {
        out_str(err, &format!("{what} must be absolute"));
        return None;
    }
    Some(path)
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
    let rc = guard(err, || {
        let Some(root) = (unsafe { abs_root(backup_root, err, "backup root") }) else {
            return 2;
        };
        let Some(pairing_root) = (unsafe { abs_root(pairing_root, err, "pairing root") }) else {
            return 2;
        };
        let mux = unsafe { opt_owned(mux_address) };
        let mux_address = (!mux.is_empty()).then_some(mux);

        let context = match EngineContext::new(root, pairing_root, mux_address.as_deref()) {
            Ok(context) => context,
            Err(message) => {
                out_str(err, &message);
                return 2;
            }
        };
        engine = Box::into_raw(Box::new(AvEngine::new(context)));
        0
    });
    if rc == 0 {
        engine
    } else {
        std::ptr::null_mut()
    }
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
        let Some(engine) = (unsafe { engine_ref(engine, std::ptr::null_mut()) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let Some(operation_id) =
            (unsafe { req_str(operation_id, std::ptr::null_mut(), "bad operation id") })
        else {
            return ErrorKind::InvalidArgument.code();
        };
        if engine.operations.cancel(&operation_id) {
            0
        } else {
            AV_CANCEL_NOT_REGISTERED
        }
    })
}

pub(crate) unsafe fn engine_ref<'a>(
    engine: *mut AvEngine,
    err: *mut *mut c_char,
) -> Option<&'a AvEngine> {
    if engine.is_null() {
        out_str(err, "engine is closed");
        None
    } else {
        Some(unsafe { &*engine })
    }
}

/// Unpacks the (engine, udid) pair every device export starts with.
pub(crate) unsafe fn engine_udid<'a>(
    engine: *mut AvEngine,
    udid: *const c_char,
    err: *mut *mut c_char,
) -> Option<(&'a AvEngine, String)> {
    let engine = unsafe { engine_ref(engine, err) }?;
    let udid = unsafe { req_str(udid, err, "bad udid") }?;
    Some((engine, udid))
}

/// Reports whether the muxer (usbmuxd/netmuxd) is reachable. The ABI's one
/// predicate-style return: 1 = up, 0 = down (a hung socket counts as down).
#[no_mangle]
pub extern "C" fn av_mux_probe(engine: *mut AvEngine) -> i32 {
    let Some(engine) = (unsafe { engine_ref(engine, std::ptr::null_mut()) }) else {
        return 0;
    };
    guard(std::ptr::null_mut(), || {
        let context = engine.context();
        let up = crate::block(async {
            crate::mux_bound(context.mux_addr().connect(0))
                .await
                .is_ok()
        });
        up as i32
    })
}
