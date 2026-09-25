//! AirVault C-ABI shim over the `idevice` crate; cbindgen generates the Go
//! bridge's checked-in C header from these exports. The rc/error contract lives
//! with the AV_ERROR_* constants in ffi.rs; strings are caller-owned
//! (`av_string_free`); JSON is camelCase (see internal/engine/engine.go). Every
//! native op is time-bounded here (Go's ctx cannot interrupt a cgo call) except
//! transfers and owned pull streams, which carry explicit cancellation handles.

use std::sync::{Mutex, MutexGuard};

mod activation;
mod afc;
mod afc_pool;
mod apps;
mod backup_storage;
mod bounded;
mod console;
mod device;
mod discover;
mod engine_error;
mod ffi;
mod lock_observer;
mod logging;
mod mobilebackup2;
mod object_store;
mod operation_registry;
mod pairing;
mod pairing_store;
mod path_sandbox;
mod power_assertion;
mod provider;
mod pull_stream;
mod timeouts;
mod transfer;
mod watch;

/// Recover a mutex guard even if a previous holder panicked — every shim lock
/// guards plain data, so a poisoned lock stays safe to use.
pub(crate) fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|poisoned| poisoned.into_inner())
}
