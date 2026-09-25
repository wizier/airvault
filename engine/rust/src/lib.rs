//! AirVault C-ABI shim over the `idevice` crate; cbindgen generates the Go
//! bridge's checked-in C header from these exports. The rc/error contract lives
//! with the AV_ERROR_* constants in ffi.rs; strings are caller-owned
//! (`av_string_free`); JSON is camelCase (see internal/engine/engine.go). Every
//! native op is time-bounded here (Go's ctx cannot interrupt a cgo call) except
//! transfers and owned pull streams, which carry explicit cancellation handles.

use std::collections::BTreeMap;
use std::ffi::{c_char, CStr, CString};
use std::fs::{File, OpenOptions};
use std::future::Future;
use std::io::{self, Read, Write};
use std::net::SocketAddr;
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::path::{Path, PathBuf};
use std::str::FromStr;
use std::sync::{LazyLock, Mutex, OnceLock};
use std::time::Duration;

use idevice::pairing_file::PairingFile;
use idevice::provider::{IdeviceProvider, UsbmuxdProvider};
use idevice::services::lockdown::LockdownClient;
use idevice::usbmuxd::{Connection, UsbmuxdAddr, UsbmuxdDevice};
use idevice::IdeviceService;
use serde_json::Value as JsonValue;
use tracing::field::{Field, Visit};
use tracing::{Event, Subscriber};
use tracing_subscriber::layer::{Context as LayerContext, SubscriberExt};
use tracing_subscriber::registry::LookupSpan;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::Layer;

use crate::engine_error::{EngineFailure, ErrorKind};

mod activation;
mod afc;
mod backup_storage;
mod bounded;
mod console;
mod discover;
mod engine_error;
mod ffi;
mod lock_observer;
mod mgmt;
mod mobilebackup2;
mod object_store;
mod operation_registry;
mod pairing;
mod path_sandbox;
mod power_assertion;
mod pull_stream;
mod timeouts;
mod transfer;
mod watch;

/// Structured tracing event callback into the Go host. Strings are borrowed
/// for the duration of the call; `fields_json` is a flat JSON object.
type LogCb = extern "C" fn(i32, *const c_char, *const c_char, *const c_char);

static LOG_CALLBACK: OnceLock<LogCb> = OnceLock::new();
static LOG_FILTER: OnceLock<String> = OnceLock::new();
static TRACING_READY: OnceLock<bool> = OnceLock::new();

#[derive(Default)]
struct LogFields(BTreeMap<String, JsonValue>);

impl Visit for LogFields {
    fn record_i64(&mut self, field: &Field, value: i64) {
        self.0.insert(field.name().to_owned(), value.into());
    }

    fn record_u64(&mut self, field: &Field, value: u64) {
        self.0.insert(field.name().to_owned(), value.into());
    }

    fn record_bool(&mut self, field: &Field, value: bool) {
        self.0.insert(field.name().to_owned(), value.into());
    }

    fn record_f64(&mut self, field: &Field, value: f64) {
        let value = serde_json::Number::from_f64(value)
            .map(JsonValue::Number)
            .unwrap_or(JsonValue::Null);
        self.0.insert(field.name().to_owned(), value);
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        self.0
            .insert(field.name().to_owned(), JsonValue::String(value.to_owned()));
    }

    fn record_debug(&mut self, field: &Field, value: &dyn std::fmt::Debug) {
        self.0.insert(
            field.name().to_owned(),
            JsonValue::String(format!("{value:?}")),
        );
    }
}

struct GoLogLayer;

impl<S> Layer<S> for GoLogLayer
where
    S: Subscriber + for<'lookup> LookupSpan<'lookup>,
{
    fn on_new_span(
        &self,
        attrs: &tracing::span::Attributes<'_>,
        id: &tracing::span::Id,
        ctx: LayerContext<'_, S>,
    ) {
        let mut fields = LogFields::default();
        attrs.record(&mut fields);
        if let Some(span) = ctx.span(id) {
            span.extensions_mut().insert(fields);
        }
    }

    fn on_record(
        &self,
        id: &tracing::span::Id,
        values: &tracing::span::Record<'_>,
        ctx: LayerContext<'_, S>,
    ) {
        if let Some(span) = ctx.span(id) {
            let mut extensions = span.extensions_mut();
            if let Some(fields) = extensions.get_mut::<LogFields>() {
                values.record(fields);
            }
        }
    }

    fn on_event(&self, event: &Event<'_>, ctx: LayerContext<'_, S>) {
        let Some(callback) = LOG_CALLBACK.get().copied() else {
            return;
        };
        let mut fields = LogFields::default();
        if let Some(scope) = ctx.event_scope(event) {
            for span in scope.from_root() {
                if let Some(span_fields) = span.extensions().get::<LogFields>() {
                    fields.0.extend(span_fields.0.clone());
                }
            }
        }
        event.record(&mut fields);

        let message = fields
            .0
            .remove("message")
            .and_then(|value| value.as_str().map(str::to_owned))
            .unwrap_or_else(|| event.metadata().name().to_owned());
        let fields_json = serde_json::to_string(&fields.0).unwrap_or_else(|_| "{}".to_owned());
        let target = ffi_cstring(event.metadata().target());
        let message = ffi_cstring(&message);
        let fields_json = ffi_cstring(&fields_json);
        callback(
            tracing_level(event.metadata().level()),
            target.as_ptr(),
            message.as_ptr(),
            fields_json.as_ptr(),
        );
    }
}

fn tracing_level(level: &tracing::Level) -> i32 {
    match *level {
        tracing::Level::ERROR => 8,
        tracing::Level::WARN => 4,
        tracing::Level::INFO => 0,
        tracing::Level::DEBUG => -4,
        tracing::Level::TRACE => -8,
    }
}

// RUST_LOG wins: dependency logs are only useful one module at a time, since
// idevice dumps AFC packets and whole plists at debug.
fn tracing_filter() -> tracing_subscriber::EnvFilter {
    if let Ok(filter) = tracing_subscriber::EnvFilter::try_from_default_env() {
        return filter;
    }
    match LOG_FILTER.get() {
        Some(filter) => tracing_subscriber::EnvFilter::new(filter),
        None => tracing_subscriber::EnvFilter::new("warn"),
    }
}

fn init_tracing() -> bool {
    *TRACING_READY.get_or_init(|| {
        if LOG_CALLBACK.get().is_some() {
            tracing_subscriber::registry()
                .with(tracing_filter())
                .with(GoLogLayer)
                .try_init()
                .is_ok()
        } else {
            tracing_subscriber::fmt()
                .with_env_filter(tracing_filter())
                .with_ansi(false)
                .with_writer(std::io::stderr)
                .try_init()
                .is_ok()
        }
    })
}

/// Connects Rust tracing to the host logger before the runtime starts. The
/// selected application level applies to this crate; dependency warnings stay
/// available without enabling their high-volume info/debug streams.
///
/// # Safety
/// `level` must be null or point to a valid NUL-terminated string for the call.
#[no_mangle]
pub unsafe extern "C" fn av_log_init(cb: LogCb, level: *const c_char) -> i32 {
    guard(std::ptr::null_mut(), || {
        let level = unsafe { in_str(level) }
            .filter(|s| !s.is_empty())
            .unwrap_or("info")
            .to_ascii_lowercase();
        let filter = format!("warn,airvault_shim={level}");
        let _ = LOG_CALLBACK.set(cb);
        let _ = LOG_FILTER.set(filter);
        if init_tracing() {
            0
        } else {
            1
        }
    })
}

/// Span shared by every durable mutation that crosses the Go/Rust boundary.
pub(crate) fn operation_span(job_id: &str, operation: &str, udid: &str) -> tracing::Span {
    let span = tracing::info_span!(
        "operation",
        job_id = tracing::field::Empty,
        operation = %operation,
        udid = %udid
    );
    if !job_id.is_empty() {
        span.record("job_id", job_id);
    }
    span
}

// Shared multi-thread tokio runtime. Go always calls in from outside any
// runtime, so block_on is safe. av_log_init normally wires tracing to Go first;
// direct Rust callers retain a stderr fallback controlled by RUST_LOG.
static RT: LazyLock<tokio::runtime::Runtime> = LazyLock::new(|| {
    let _ = init_tracing();
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("tokio runtime")
});

pub(crate) fn block<F: Future>(f: F) -> F::Output {
    RT.block_on(f)
}

/// Long-lived stream workers run as tasks on the shared runtime, not on
/// dedicated OS threads; close() joins them via block().
pub(crate) fn spawn<F>(f: F) -> tokio::task::JoinHandle<F::Output>
where
    F: Future + Send + 'static,
    F::Output: Send + 'static,
{
    RT.spawn(f)
}

/// Immutable provider and storage context shared by one explicit AvEngine.
/// Protocol code receives it as an ordinary dependency; process environment is
/// never consulted after construction.
#[derive(Debug)]
pub(crate) struct EngineContext {
    backup_root: PathBuf,
    pairing_store: PairingStore,
    mux_addr: UsbmuxdAddr,
}

impl EngineContext {
    pub(crate) fn new(
        backup_root: PathBuf,
        pairing_root: PathBuf,
        mux_address: Option<&str>,
    ) -> Result<Self, String> {
        let mux_addr = match mux_address.filter(|value| !value.is_empty()) {
            None => UsbmuxdAddr::default(),
            Some(value) => {
                #[cfg(unix)]
                {
                    if value.contains(':') {
                        UsbmuxdAddr::TcpSocket(
                            SocketAddr::from_str(value)
                                .map_err(|error| format!("invalid mux address: {error}"))?,
                        )
                    } else {
                        UsbmuxdAddr::UnixSocket(value.to_owned())
                    }
                }
                #[cfg(not(unix))]
                {
                    UsbmuxdAddr::TcpSocket(
                        SocketAddr::from_str(value)
                            .map_err(|error| format!("invalid mux address: {error}"))?,
                    )
                }
            }
        };
        Ok(Self {
            backup_root,
            pairing_store: PairingStore::new(pairing_root),
            mux_addr,
        })
    }

    pub(crate) fn backup_root(&self) -> &Path {
        &self.backup_root
    }

    pub(crate) fn mux_addr(&self) -> UsbmuxdAddr {
        self.mux_addr.clone()
    }
}

/// One crate-wide UDID length cap (matches the Go/store source limit).
pub(crate) const MAX_UDID_BYTES: usize = 64;

/// A muxer conversation (connect + query). Its failure decides the outcome, but
/// a usbmuxd socket that accepts and then hangs must never block the daemon's
/// single refresh worker.
const MUX_TIMEOUT: Duration = Duration::from_secs(5);

/// Run a muxer conversation under MUX_TIMEOUT, flattening elapsed into an error.
pub(crate) async fn mux_bound<T>(
    f: impl Future<Output = Result<T, idevice::IdeviceError>>,
) -> Result<T, idevice::IdeviceError> {
    tokio::time::timeout(MUX_TIMEOUT, f)
        .await
        .unwrap_or(Err(idevice::IdeviceError::Timeout))
}

/// Convert UTF-8 into an owned C string without discarding diagnostics that
/// contain an interior NUL. C cannot represent that byte, so expose it as the
/// conventional visible `\\0` escape instead.
pub(crate) fn ffi_cstring(s: &str) -> CString {
    let mut bytes = Vec::with_capacity(s.len());
    for byte in s.bytes() {
        if byte == 0 {
            bytes.extend_from_slice(b"\\0");
        } else {
            bytes.push(byte);
        }
    }
    // SAFETY: every input NUL was replaced with two non-NUL ASCII bytes, and
    // no other byte in a UTF-8 string can be zero.
    unsafe { CString::from_vec_unchecked(bytes) }
}

/// Write an owned C string into an out-param (caller frees via av_string_free).
pub(crate) fn out_str(dst: *mut *mut c_char, s: &str) {
    if !dst.is_null() {
        let c = ffi_cstring(s);
        unsafe { *dst = c.into_raw() };
    }
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

/// The one path from a failure to the ABI: detail into `err`, kind as the rc.
pub(crate) fn write_failure(err: *mut *mut c_char, failure: EngineFailure) -> i32 {
    out_str(err, &failure.detail);
    failure.kind.code()
}

/// JSON-encode a response payload. The exported caller's guard contains the
/// impossible serializer panic before it can cross the C ABI.
pub(crate) fn to_json<T: serde::Serialize>(v: &T) -> String {
    serde_json::to_string(v).expect("shim payloads always serialize")
}

/// Recover a mutex guard even if a previous holder panicked — every shim lock
/// guards plain data, so a poisoned lock stays safe to use.
pub(crate) fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// Await `producer` under `limit`: its failure, or an elapsed `timeout_msg` as
/// AV_ERROR_TIMEOUT, goes to `err`; its success value passes through `on_ok`,
/// which returns the success rc.
pub(crate) fn block_bounded<T, E: Into<EngineFailure>>(
    err: *mut *mut c_char,
    limit: Duration,
    timeout_msg: &str,
    producer: impl Future<Output = Result<T, E>>,
    on_ok: impl FnOnce(T) -> i32,
) -> i32 {
    block(async {
        match tokio::time::timeout(limit, producer).await {
            Ok(Ok(value)) => on_ok(value),
            Ok(Err(error)) => write_failure(err, error.into()),
            Err(_) => write_failure(err, EngineFailure::new(ErrorKind::Timeout, timeout_msg)),
        }
    })
}

/// Bounded read that writes its Ok string to `out` (rc 0).
pub(crate) fn block_bounded_out<E: Into<EngineFailure>>(
    out: *mut *mut c_char,
    err: *mut *mut c_char,
    limit: Duration,
    timeout_msg: &str,
    producer: impl Future<Output = Result<String, E>>,
) -> i32 {
    block_bounded(err, limit, timeout_msg, producer, move |payload| {
        out_str(out, &payload);
        0
    })
}

/// block_bounded_out without an output payload: rc 0 on success.
pub(crate) fn block_bounded_unit<E: Into<EngineFailure>>(
    err: *mut *mut c_char,
    limit: Duration,
    timeout_msg: &str,
    producer: impl Future<Output = Result<(), E>>,
) -> i32 {
    block_bounded(err, limit, timeout_msg, producer, |()| 0)
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

/// Read a required (non-empty) C-string argument; on failure writes `what` to
/// `err` and returns None — the caller then returns AV_ERROR_INVALID_ARGUMENT.
pub(crate) unsafe fn req_str(
    p: *const c_char,
    err: *mut *mut c_char,
    what: &str,
) -> Option<String> {
    match in_str(p) {
        Some(s) if !s.is_empty() => Some(s.to_owned()),
        _ => {
            out_str(err, what);
            None
        }
    }
}

/// The muxer lists one entry per transport (USB + Wi-Fi = twice per udid);
/// collapse to one entry per device, preferring USB — faster, more reliable,
/// and some operations only make sense over the cable.
fn dedupe_prefer_usb(devices: Vec<UsbmuxdDevice>) -> Vec<UsbmuxdDevice> {
    let mut out: Vec<UsbmuxdDevice> = Vec::with_capacity(devices.len());
    for d in devices {
        match out.iter_mut().find(|e| e.udid == d.udid) {
            Some(e) => {
                if matches!(d.connection_type, Connection::Usb) {
                    *e = d;
                }
            }
            None => out.push(d),
        }
    }
    out
}

/// Devices currently on the muxer, one entry per device (USB preferred).
/// Time-bounded: every caller (discover/list/usb_list/provider_for and the
/// watch loop's snapshots) inherits the muxer cap from here.
pub(crate) async fn devices_deduped(
    context: &EngineContext,
) -> Result<Vec<UsbmuxdDevice>, idevice::IdeviceError> {
    mux_bound(async {
        let mut mux = context.mux_addr().connect(0).await?;
        Ok(dedupe_prefer_usb(mux.get_devices().await?))
    })
    .await
}

// AirVault's own pairing-record store (one plist per udid under EngineContext's
// pairing root) — not the system usbmuxd/Finder store — answers "did AirVault
// pair this". Records hold private keys: 0600 files, 0700 dirs, atomic durable writes.

const MAX_PAIRING_RECORD_BYTES: u64 = 4 * 1024 * 1024;
const MAX_PAIRING_IDENTITY_BYTES: u64 = 16 * 1024;
const PENDING_PAIRING_DIR: &str = ".pending";

#[derive(Debug, thiserror::Error)]
pub(crate) enum PairingStoreError {
    #[error("invalid device identifier {0:?}")]
    InvalidUdid(String),
    #[error("invalid pairing record {}: {detail}", path.display())]
    InvalidRecord { path: PathBuf, detail: String },
    #[error("unsafe pairing-store path {}: {detail}", path.display())]
    UnsafePath { path: PathBuf, detail: String },
    #[error("cannot {operation} {}: {source}", path.display())]
    Io {
        operation: &'static str,
        path: PathBuf,
        #[source]
        source: io::Error,
    },
}

impl PairingStoreError {
    fn io(operation: &'static str, path: impl Into<PathBuf>, source: io::Error) -> Self {
        Self::Io {
            operation,
            path: path.into(),
            source,
        }
    }
}

#[derive(Clone, Debug, serde::Deserialize, serde::Serialize)]
pub(crate) struct PairingIdentity {
    #[serde(rename = "HostID")]
    pub(crate) host_id: String,
    #[serde(rename = "SystemBUID")]
    pub(crate) system_buid: String,
}

impl PairingIdentity {
    pub(crate) fn new(host_id: String, system_buid: String) -> Self {
        Self {
            host_id,
            system_buid,
        }
    }

    fn validate(self, path: &Path) -> Result<Self, PairingStoreError> {
        if self.host_id.trim().is_empty() || self.system_buid.trim().is_empty() {
            return Err(PairingStoreError::InvalidRecord {
                path: path.to_owned(),
                detail: "HostID and SystemBUID must both be non-empty".into(),
            });
        }
        Ok(self)
    }
}

#[derive(Debug)]
struct PairingStore {
    root: PathBuf,
}

impl PairingStore {
    fn new(root: impl Into<PathBuf>) -> Self {
        Self { root: root.into() }
    }

    fn file_name(udid: &str) -> Result<String, PairingStoreError> {
        if udid.is_empty()
            || udid.len() > MAX_UDID_BYTES
            || !udid
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_'))
        {
            return Err(PairingStoreError::InvalidUdid(udid.to_owned()));
        }
        Ok(format!("{udid}.plist"))
    }

    fn pairing_path(&self, udid: &str) -> Result<PathBuf, PairingStoreError> {
        Ok(self.root.join(Self::file_name(udid)?))
    }

    fn pending_dir(&self) -> PathBuf {
        self.root.join(PENDING_PAIRING_DIR)
    }

    fn identity_path(&self, udid: &str) -> Result<PathBuf, PairingStoreError> {
        Ok(self.pending_dir().join(Self::file_name(udid)?))
    }

    fn ensure_root(&self, create: bool) -> Result<bool, PairingStoreError> {
        ensure_private_dir(&self.root, create)
    }

    fn ensure_pending_dir(&self, create: bool) -> Result<bool, PairingStoreError> {
        if !self.ensure_root(create)? {
            return Ok(false);
        }
        ensure_private_dir(&self.pending_dir(), create)
    }

    /// Our pairing record for `udid`, if we paired it. Missing and unreadable are
    /// deliberately different: corrupt private material must never look unpaired.
    fn load_pairing(&self, udid: &str) -> Result<Option<PairingFile>, PairingStoreError> {
        let path = self.pairing_path(udid)?;
        if !self.ensure_root(false)? {
            return Ok(None);
        }
        let Some(bytes) = read_private_file(&path, MAX_PAIRING_RECORD_BYTES)? else {
            return Ok(None);
        };
        PairingFile::from_bytes(&bytes)
            .map(Some)
            .map_err(|e| PairingStoreError::InvalidRecord {
                path,
                detail: format!("{e:?}"),
            })
    }

    /// Persists our pairing record with temp + fsync + rename + directory fsync.
    fn save_pairing(&self, udid: &str, pairing: &PairingFile) -> Result<(), PairingStoreError> {
        let path = self.pairing_path(udid)?;
        let bytes = pairing
            .clone()
            .serialize()
            .map_err(|e| PairingStoreError::InvalidRecord {
                path: path.clone(),
                detail: format!("cannot serialize record: {e:?}"),
            })?;
        ensure_size(&path, bytes.len() as u64, MAX_PAIRING_RECORD_BYTES)?;
        self.ensure_root(true)?;
        write_atomic_private(&self.root, &path, &bytes)
    }

    /// Durably forgets our pairing record for `udid`.
    fn delete_pairing(&self, udid: &str) -> Result<bool, PairingStoreError> {
        let path = self.pairing_path(udid)?;
        if !self.ensure_root(false)? {
            return Ok(false);
        }
        remove_private_file(&self.root, &path)
    }

    fn load_identity(&self, udid: &str) -> Result<Option<PairingIdentity>, PairingStoreError> {
        let path = self.identity_path(udid)?;
        if !self.ensure_pending_dir(false)? {
            return Ok(None);
        }
        let Some(bytes) = read_private_file(&path, MAX_PAIRING_IDENTITY_BYTES)? else {
            return Ok(None);
        };
        let identity: PairingIdentity =
            plist::from_bytes(&bytes).map_err(|e| PairingStoreError::InvalidRecord {
                path: path.clone(),
                detail: format!("cannot decode pending pairing identity: {e}"),
            })?;
        identity.validate(&path).map(Some)
    }

    fn persist_identity_if_absent(
        &self,
        udid: &str,
        candidate: PairingIdentity,
    ) -> Result<PairingIdentity, PairingStoreError> {
        let _guard = lock(&PAIRING_IDENTITY_LOCK);
        if let Some(identity) = self.load_identity(udid)? {
            return Ok(identity);
        }
        let path = self.identity_path(udid)?;
        let candidate = candidate.validate(&path)?;
        let mut bytes = Vec::new();
        plist::to_writer_xml(&mut bytes, &candidate).map_err(|e| {
            PairingStoreError::InvalidRecord {
                path: path.clone(),
                detail: format!("cannot encode pending pairing identity: {e}"),
            }
        })?;
        ensure_size(&path, bytes.len() as u64, MAX_PAIRING_IDENTITY_BYTES)?;
        self.ensure_pending_dir(true)?;
        write_atomic_private(&self.pending_dir(), &path, &bytes)?;
        Ok(candidate)
    }

    fn delete_identity(&self, udid: &str) -> Result<bool, PairingStoreError> {
        let path = self.identity_path(udid)?;
        if !self.ensure_pending_dir(false)? {
            return Ok(false);
        }
        remove_private_file(&self.pending_dir(), &path)
    }
}

static PAIRING_IDENTITY_LOCK: LazyLock<Mutex<()>> = LazyLock::new(|| Mutex::new(()));

/// One byte-limit gate for every private-record read and write path.
fn ensure_size(path: &Path, len: u64, max_bytes: u64) -> Result<(), PairingStoreError> {
    if len > max_bytes {
        return Err(PairingStoreError::InvalidRecord {
            path: path.to_owned(),
            detail: format!("record exceeds {max_bytes}-byte limit"),
        });
    }
    Ok(())
}

fn ensure_private_dir(path: &Path, create: bool) -> Result<bool, PairingStoreError> {
    let mut created = false;
    let metadata = match std::fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(e) if e.kind() == io::ErrorKind::NotFound && !create => return Ok(false),
        Err(e) if e.kind() == io::ErrorKind::NotFound => {
            std::fs::create_dir_all(path)
                .map_err(|source| PairingStoreError::io("create directory", path, source))?;
            created = true;
            std::fs::symlink_metadata(path)
                .map_err(|source| PairingStoreError::io("inspect directory", path, source))?
        }
        Err(source) => return Err(PairingStoreError::io("inspect directory", path, source)),
    };
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err(PairingStoreError::UnsafePath {
            path: path.to_owned(),
            detail: "expected a real directory, not a symlink or other file".into(),
        });
    }
    set_private_mode(path, &metadata, 0o700)?;
    if created {
        sync_dir(path)?;
        if let Some(parent) = path
            .parent()
            .filter(|parent| !parent.as_os_str().is_empty())
        {
            sync_dir(parent)?;
        }
    }
    Ok(true)
}

fn read_private_file(path: &Path, max_bytes: u64) -> Result<Option<Vec<u8>>, PairingStoreError> {
    let metadata = match std::fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(source) => return Err(PairingStoreError::io("inspect file", path, source)),
    };
    if metadata.file_type().is_symlink() || !metadata.is_file() {
        return Err(PairingStoreError::UnsafePath {
            path: path.to_owned(),
            detail: "expected a regular file, not a symlink or other file".into(),
        });
    }
    ensure_size(path, metadata.len(), max_bytes)?;
    set_private_mode(path, &metadata, 0o600)?;
    let file =
        File::open(path).map_err(|source| PairingStoreError::io("open file", path, source))?;
    let mut bytes = Vec::with_capacity(metadata.len() as usize);
    file.take(max_bytes + 1)
        .read_to_end(&mut bytes)
        .map_err(|source| PairingStoreError::io("read file", path, source))?;
    ensure_size(path, bytes.len() as u64, max_bytes)?;
    Ok(Some(bytes))
}

fn write_atomic_private(dir: &Path, path: &Path, bytes: &[u8]) -> Result<(), PairingStoreError> {
    let file_name = path
        .file_name()
        .and_then(|name| name.to_str())
        .ok_or_else(|| PairingStoreError::UnsafePath {
            path: path.to_owned(),
            detail: "record has no valid UTF-8 filename".into(),
        })?;
    let temp = dir.join(format!(
        ".{file_name}.tmp-{}-{}",
        std::process::id(),
        uuid::Uuid::new_v4()
    ));
    let result = (|| {
        let mut options = OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut file = options
            .open(&temp)
            .map_err(|source| PairingStoreError::io("create temporary file", &temp, source))?;
        file.write_all(bytes)
            .map_err(|source| PairingStoreError::io("write temporary file", &temp, source))?;
        file.sync_all()
            .map_err(|source| PairingStoreError::io("sync temporary file", &temp, source))?;
        drop(file);
        std::fs::rename(&temp, path)
            .map_err(|source| PairingStoreError::io("replace record", path, source))?;
        sync_dir(dir)
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&temp);
    }
    result
}

fn remove_private_file(dir: &Path, path: &Path) -> Result<bool, PairingStoreError> {
    match std::fs::remove_file(path) {
        Ok(()) => {
            sync_dir(dir)?;
            Ok(true)
        }
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(source) => Err(PairingStoreError::io("remove file", path, source)),
    }
}

#[cfg(unix)]
fn set_private_mode(
    path: &Path,
    metadata: &std::fs::Metadata,
    expected: u32,
) -> Result<(), PairingStoreError> {
    use std::os::unix::fs::PermissionsExt;

    if metadata.permissions().mode() & 0o777 != expected {
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(expected))
            .map_err(|source| PairingStoreError::io("set permissions on", path, source))?;
        if metadata.is_dir() {
            sync_dir(path)?;
        } else {
            File::open(path)
                .and_then(|file| file.sync_all())
                .map_err(|source| PairingStoreError::io("sync permissions on", path, source))?;
        }
    }
    Ok(())
}

#[cfg(not(unix))]
fn set_private_mode(
    _path: &Path,
    _metadata: &std::fs::Metadata,
    _expected: u32,
) -> Result<(), PairingStoreError> {
    Ok(())
}

#[cfg(unix)]
fn sync_dir(path: &Path) -> Result<(), PairingStoreError> {
    File::open(path)
        .and_then(|dir| dir.sync_all())
        .map_err(|source| PairingStoreError::io("sync directory", path, source))
}

#[cfg(not(unix))]
fn sync_dir(_path: &Path) -> Result<(), PairingStoreError> {
    Ok(())
}

/// Transports via usbmuxd but authenticates with AirVault's OWN pairing record;
/// get_pairing_file errors when we have none — which is how a device we never
/// paired reads as unpaired.
#[derive(Debug)]
pub(crate) struct AirvaultProvider {
    inner: UsbmuxdProvider,
    pairing: StoredPairing,
}

#[derive(Debug)]
enum StoredPairing {
    Missing,
    Present(Box<PairingFile>),
    Invalid(String),
}

impl IdeviceProvider for AirvaultProvider {
    fn connect(
        &self,
        port: u16,
    ) -> std::pin::Pin<
        Box<
            dyn std::future::Future<Output = Result<idevice::Idevice, idevice::IdeviceError>>
                + Send,
        >,
    > {
        self.inner.connect(port)
    }
    fn label(&self) -> &str {
        self.inner.label()
    }
    fn get_pairing_file(
        &self,
    ) -> std::pin::Pin<
        Box<dyn std::future::Future<Output = Result<PairingFile, idevice::IdeviceError>> + Send>,
    > {
        let pairing = match &self.pairing {
            StoredPairing::Missing => Err(idevice::IdeviceError::InvalidHostID),
            StoredPairing::Present(pairing) => Ok(pairing.as_ref().clone()),
            StoredPairing::Invalid(detail) => Err(idevice::IdeviceError::UnexpectedResponse(
                format!("AirVault pairing record is unavailable: {detail}"),
            )),
        };
        Box::pin(async move { pairing })
    }
}

/// Picks the USB entry when the device is on both transports, so every
/// lockdown/mb2 conversation rides the cable whenever one is plugged in. The
/// resulting provider authenticates with AirVault's own pairing record.
pub(crate) async fn provider_for(
    context: &EngineContext,
    udid: &str,
) -> Result<AirvaultProvider, idevice::IdeviceError> {
    let dev = devices_deduped(context)
        .await?
        .into_iter()
        .find(|d| d.udid == udid)
        .ok_or(idevice::IdeviceError::DeviceNotFound)?;
    let pairing = match context.pairing_store.load_pairing(udid) {
        Ok(Some(pairing)) => StoredPairing::Present(Box::new(pairing)),
        Ok(None) => StoredPairing::Missing,
        Err(e) => StoredPairing::Invalid(e.to_string()),
    };
    Ok(AirvaultProvider {
        inner: dev.to_provider(context.mux_addr(), "AirVault"),
        pairing,
    })
}

/// Wire label for a connection kind. Unknown is reported as usb (a locally
/// attached device we can't classify), never dropped.
pub(crate) fn conn_str(c: &Connection) -> &'static str {
    match c {
        Connection::Usb => "usb",
        Connection::Network(_) => "wifi",
        Connection::Unknown(_) => "usb",
    }
}

/// Connect lockdown and open an authenticated session with our pairing record —
/// the preamble every session-gated domain read shares.
pub(crate) async fn authed_lockdown(
    provider: &AirvaultProvider,
) -> Result<LockdownClient, idevice::IdeviceError> {
    let mut lc = LockdownClient::connect(provider).await?;
    let pf = provider.get_pairing_file().await?;
    lc.start_session(&pf).await?;
    Ok(lc)
}

/// The authenticated start-service dance shared by single-connection daemons.
/// mobilebackup2 keeps its own variant (escrow bag + plain-session retry).
pub(crate) async fn connect_service(
    provider: &AirvaultProvider,
    service: &str,
) -> Result<idevice::Idevice, idevice::IdeviceError> {
    let pf = provider.get_pairing_file().await?;
    let mut lockdown = LockdownClient::connect(provider).await?;
    let legacy = lockdown.start_session(&pf).await?;
    let (port, ssl) = lockdown.start_service(service).await?;
    let mut connection = provider.connect(port).await?;
    if ssl {
        connection.start_session(&pf, legacy).await?;
    }
    Ok(connection)
}

/// The one lockdown read shared by discovery and the backup-password flow.
pub(crate) async fn read_will_encrypt(
    lc: &mut LockdownClient,
) -> Result<bool, idevice::IdeviceError> {
    lc.get_value(Some("WillEncrypt"), Some("com.apple.mobile.backup"))
        .await?
        .as_boolean()
        .ok_or_else(|| {
            idevice::IdeviceError::UnexpectedResponse(
                "lockdown WillEncrypt is not a boolean".into(),
            )
        })
}

pub(crate) async fn getv_str(lc: &mut LockdownClient, key: &str) -> String {
    lc.get_value(Some(key), None)
        .await
        .ok()
        .and_then(|v| v.as_string().map(str::to_owned))
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;

    type CapturedLog = (i32, String, String, String);
    static CAPTURED_LOG: Mutex<Option<CapturedLog>> = Mutex::new(None);

    extern "C" fn capture_log(
        level: i32,
        target: *const c_char,
        message: *const c_char,
        fields: *const c_char,
    ) {
        let copy = |value| unsafe { CStr::from_ptr(value).to_string_lossy().into_owned() };
        *CAPTURED_LOG.lock().unwrap() = Some((level, copy(target), copy(message), copy(fields)));
    }

    #[test]
    fn go_log_layer_forwards_span_and_event_fields() {
        assert!(LOG_CALLBACK.set(capture_log).is_ok());
        let subscriber = tracing_subscriber::registry().with(GoLogLayer);
        tracing::subscriber::with_default(subscriber, || {
            let span = tracing::info_span!("operation", job_id = "job-123", udid = "device-1");
            span.in_scope(|| tracing::warn!(error = "retryable", "retry scheduled"));
        });

        let captured = CAPTURED_LOG.lock().unwrap().take().unwrap();
        assert_eq!(captured.0, 4);
        assert_eq!(captured.2, "retry scheduled");
        let fields: BTreeMap<String, JsonValue> = serde_json::from_str(&captured.3).unwrap();
        assert_eq!(fields["job_id"], "job-123");
        assert_eq!(fields["udid"], "device-1");
        assert_eq!(fields["error"], "retryable");
    }

    #[cfg(unix)]
    #[test]
    fn engine_contexts_keep_provider_configuration_independent() {
        let first = EngineContext::new(
            PathBuf::from("/tmp/backups-one"),
            PathBuf::from("/tmp/pairing-one"),
            Some("/tmp/mux-one"),
        )
        .unwrap();
        let second = EngineContext::new(
            PathBuf::from("/tmp/backups-two"),
            PathBuf::from("/tmp/pairing-two"),
            Some("/tmp/mux-two"),
        )
        .unwrap();

        assert_eq!(first.backup_root(), Path::new("/tmp/backups-one"));
        assert_eq!(second.backup_root(), Path::new("/tmp/backups-two"));
        assert_eq!(first.pairing_store.root, Path::new("/tmp/pairing-one"));
        assert_eq!(second.pairing_store.root, Path::new("/tmp/pairing-two"));
        assert!(matches!(
            first.mux_addr(),
            UsbmuxdAddr::UnixSocket(path) if path == "/tmp/mux-one"
        ));
        assert!(matches!(
            second.mux_addr(),
            UsbmuxdAddr::UnixSocket(path) if path == "/tmp/mux-two"
        ));
    }
}
