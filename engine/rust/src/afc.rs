//! Stateful Apple File Conduit transport. Go owns paths, pagination, copying
//! and progress; this module owns only AFC connections and cancellable reads.
//! Connections come from and return to the engine's idle pool (afc_pool.rs).

use std::ffi::c_char;
use std::future::Future;
use std::ops::{Deref, DerefMut};
use std::ptr;
use std::sync::{Arc, Mutex};

use idevice::services::afc::errors::AfcError;
use idevice::services::afc::file::OwnedFileDescriptor;
use idevice::services::afc::{
    opcode::{AfcFopenMode, AfcOpcode},
    AfcClient, FileInfo,
};
use idevice::services::house_arrest::HouseArrestClient;
use idevice::usbmuxd::UsbmuxdDevice;
use idevice::{IdeviceError, IdeviceService};
use tokio::io::AsyncSeekExt;
use tokio_util::sync::CancellationToken;

use crate::afc_pool::{ClientOrigin, PoolKey, Transport};
use crate::bounded::{cancel_or_timeout, Interrupt};
use crate::engine_error::{EngineFailure, ErrorKind};
use crate::ffi::{block_bounded, engine_ref, guard_error, out_str, to_json, AvEngine, AvError};
use crate::provider::{block, devices_deduped, provider_from, EngineContext, MAX_UDID_BYTES};
use crate::timeouts;

const AFC_SOURCE_MEDIA: i32 = 0;
const AFC_SOURCE_APP_DOCUMENTS: i32 = 1;
const MAX_BUNDLE_BYTES: usize = 512;
const MAX_PATH_BYTES: usize = 4096;
const MAX_READ_BYTES: usize = 1024 * 1024;

struct SlotState<T> {
    resource: Option<T>,
    closed: bool,
}

// One lock is the linearization point for operation completion and cancel.
// A resource taken by an operation can only be returned while the slot is
// still open, so cancellation can never be followed by resource resurrection.
struct Slot<T> {
    state: Mutex<SlotState<T>>,
    cancel: CancellationToken,
}

impl<T> Slot<T> {
    fn new(resource: T) -> Self {
        Self {
            state: Mutex::new(SlotState {
                resource: Some(resource),
                closed: false,
            }),
            cancel: CancellationToken::new(),
        }
    }

    fn take(&self) -> Option<T> {
        let mut state = crate::lock(&self.state);
        if state.closed {
            return None;
        }
        state.resource.take()
    }

    fn put(&self, resource: T) -> Result<(), T> {
        let mut state = crate::lock(&self.state);
        if state.closed || state.resource.is_some() {
            return Err(resource);
        }
        state.resource = Some(resource);
        Ok(())
    }

    /// Interrupts an operation in flight and refuses later ones. A resource
    /// taken by that operation can no longer be put back, so it is dropped; an
    /// idle resource stays for close(), which may still reuse it.
    fn cancel(&self) {
        crate::lock(&self.state).closed = true;
        self.cancel.cancel();
    }

    /// Cancels, then hands the idle resource (if any) to the caller.
    fn close(&self) -> Option<T> {
        self.cancel();
        crate::lock(&self.state).resource.take()
    }
}

enum Resource {
    Session(AfcClient),
    File(FileGuard),
}

impl Slot<Resource> {
    fn take_session(&self) -> Result<AfcClient, EngineFailure> {
        match self.take() {
            Some(Resource::Session(client)) => Ok(client),
            Some(other) => {
                let _ = self.put(other);
                Err(cancelled("AFC handle is not a session"))
            }
            None => Err(cancelled("AFC session is closed")),
        }
    }

    fn take_file(&self) -> Result<FileGuard, EngineFailure> {
        match self.take() {
            Some(Resource::File(file)) => Ok(file),
            Some(other) => {
                let _ = self.put(other);
                Err(cancelled("AFC handle is not a file"))
            }
            None => Err(cancelled("AFC file is closed")),
        }
    }

    /// Runs one operation on a taken resource under the shared cancel/timeout
    /// budget. The operation hands back the resource to keep, or None when the
    /// transport is lost with the failure and the slot must close.
    fn run<T>(
        &self,
        cancelled_detail: &str,
        timeout_detail: &str,
        operation: impl Future<Output = (Option<Resource>, Result<T, EngineFailure>)>,
    ) -> Result<T, EngineFailure> {
        match block(cancel_or_timeout(
            &self.cancel,
            timeouts::DEVICE_WORK,
            operation,
        )) {
            Ok((Some(resource), result)) => {
                if self.put(resource).is_err() {
                    return Err(cancelled(cancelled_detail));
                }
                result
            }
            Ok((None, result)) => {
                self.cancel();
                result
            }
            Err(Interrupt::Cancelled) => {
                self.cancel();
                Err(cancelled(cancelled_detail))
            }
            Err(Interrupt::TimedOut) => {
                self.cancel();
                Err(EngineFailure::new(ErrorKind::Timeout, timeout_detail))
            }
        }
    }

    /// Runs one operation on the idle session, which it hands back for reuse.
    fn run_session<T, E: Into<EngineFailure>, Fut>(
        &self,
        timeout_detail: &str,
        operation: impl FnOnce(AfcClient) -> Fut,
    ) -> Result<T, EngineFailure>
    where
        Fut: Future<Output = (Option<AfcClient>, Result<T, E>)>,
    {
        let client = self.take_session()?;
        self.run("AFC session cancelled", timeout_detail, async {
            let (client, result) = operation(client).await;
            (client.map(Resource::Session), result.map_err(Into::into))
        })
    }
}

/// Makes an owned AFC descriptor drop-safe. The upstream type asserts when
/// dropped without a successful close (even a failed or interrupted one), so an
/// unclosed guard drops the transport instead; the device reclaims the FD then.
pub(crate) struct FileGuard(Option<OwnedFileDescriptor>);

impl FileGuard {
    pub(crate) fn new(file: OwnedFileDescriptor) -> Self {
        Self(Some(file))
    }

    /// Returns the transport only after the device confirms FileClose. Dropping
    /// this future at any point closes the transport instead of reusing it.
    pub(crate) async fn close(mut self) -> Result<AfcClient, IdeviceError> {
        let file = self.0.take().expect("AFC file guard is armed");
        let fd = file.as_raw_fd();
        // SAFETY: the guard owns the descriptor and marks it handled by
        // consuming it here. Interruption below drops the extracted transport.
        let mut client = unsafe { file.get_inner_afc() };
        client
            .send_op(AfcOpcode::FileClose, fd.to_le_bytes().to_vec(), Vec::new())
            .await?;
        Ok(client)
    }
}

impl Deref for FileGuard {
    type Target = OwnedFileDescriptor;

    fn deref(&self) -> &OwnedFileDescriptor {
        self.0.as_ref().expect("AFC file guard is armed")
    }
}

impl DerefMut for FileGuard {
    fn deref_mut(&mut self) -> &mut OwnedFileDescriptor {
        self.0.as_mut().expect("AFC file guard is armed")
    }
}

impl Drop for FileGuard {
    fn drop(&mut self) {
        if let Some(file) = self.0.take() {
            // SAFETY: this guard uniquely owns the descriptor. Extracting and
            // dropping its client closes the dedicated AFC transport, so the
            // device reclaims the still-open FD.
            drop(unsafe { file.get_inner_afc() });
        }
    }
}

/// Type-distinct FFI wrappers prevent a session from being passed to a file
/// operation (or vice versa). Both carry the connection's pool origin, so
/// closing either returns a healthy idle connection to the pool.
pub struct AvAfcSession {
    // File-open moves the slot to AvAfcFile while this wrapper stays alive for
    // the rest of the FFI call, so a concurrent cancel cannot free it early.
    slot: Mutex<Option<Arc<Slot<Resource>>>>,
    origin: ClientOrigin,
}

pub struct AvAfcFile {
    slot: Arc<Slot<Resource>>,
    origin: ClientOrigin,
}

impl AvAfcSession {
    fn new(client: AfcClient, origin: ClientOrigin) -> Self {
        Self {
            slot: Mutex::new(Some(Arc::new(Slot::new(Resource::Session(client))))),
            origin,
        }
    }

    fn slot(&self) -> Option<Arc<Slot<Resource>>> {
        crate::lock(&self.slot).clone()
    }

    fn detach(&self, expected: &Arc<Slot<Resource>>) -> bool {
        let mut slot = crate::lock(&self.slot);
        if slot
            .as_ref()
            .is_some_and(|current| Arc::ptr_eq(current, expected))
        {
            slot.take();
            true
        } else {
            false
        }
    }
}

fn cancelled(detail: &str) -> EngineFailure {
    EngineFailure::new(ErrorKind::Cancelled, detail)
}

/// The live slot behind a session handle, or Cancelled once it is closed.
unsafe fn session_slot(session: *mut AvAfcSession) -> Result<Arc<Slot<Resource>>, EngineFailure> {
    unsafe { session.as_ref() }
        .and_then(AvAfcSession::slot)
        .ok_or_else(|| cancelled("AFC session is closed"))
}

/// Zeroes a required string out-param before the call can fail.
fn reset_out(out: *mut *mut c_char, what: &str) -> Result<(), EngineFailure> {
    if out.is_null() {
        return Err(EngineFailure::invalid_argument(what));
    }
    unsafe { *out = ptr::null_mut() };
    Ok(())
}

/// Checks the caller's read buffer and zeroes its byte count.
fn reset_read(
    buffer: *mut u8,
    buffer_len: usize,
    out_read: *mut usize,
) -> Result<(), EngineFailure> {
    if buffer.is_null() || out_read.is_null() || buffer_len == 0 || buffer_len > MAX_READ_BYTES {
        return Err(EngineFailure::invalid_argument("bad AFC read buffer"));
    }
    unsafe { *out_read = 0 };
    Ok(())
}

/// Copies read bytes into the caller's buffer (already bounded by the caller).
unsafe fn copy_read(bytes: &[u8], buffer: *mut u8, out_read: *mut usize) {
    unsafe {
        ptr::copy_nonoverlapping(bytes.as_ptr(), buffer, bytes.len());
        *out_read = bytes.len();
    }
}

unsafe fn input(
    ptr: *const u8,
    len: usize,
    max: usize,
    label: &str,
    allow_empty: bool,
) -> Result<String, EngineFailure> {
    let bad = |detail: String| Err(EngineFailure::invalid_argument(detail));
    if len > max || (ptr.is_null() && len != 0) {
        return bad(format!("bad {label}"));
    }
    if len == 0 {
        return if allow_empty {
            Ok(String::new())
        } else {
            bad(format!("bad {label}"))
        };
    }
    // SAFETY: the caller promises a readable buffer for the duration of this
    // synchronous FFI call; length was bounded above.
    let bytes = unsafe { std::slice::from_raw_parts(ptr, len) };
    if bytes.contains(&0) {
        return bad(format!("bad {label}: contains NUL"));
    }
    match std::str::from_utf8(bytes) {
        Ok(value) => Ok(value.to_owned()),
        Err(_) => bad(format!("bad {label}: not UTF-8")),
    }
}

// Go (devicefs) owns path policy — component structure and . / .. rejection.
// Here only the raw-pointer memory safety in input() plus an absolute-path
// shape check remain; the AFC OS jail confines every path regardless.
unsafe fn physical_path(ptr: *const u8, len: usize) -> Result<String, EngineFailure> {
    let path = unsafe { input(ptr, len, MAX_PATH_BYTES, "AFC path", false) }?;
    if !path.starts_with('/') {
        return Err(EngineFailure::invalid_argument("bad AFC path"));
    }
    Ok(path)
}

#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub(crate) enum Source {
    Media,
    /// One app's Documents container, by bundle id.
    AppDocuments(String),
}

fn parse_source(source: i32, bundle: String) -> Result<Source, EngineFailure> {
    let bad = |detail: &str| Err(EngineFailure::invalid_argument(detail));
    match source {
        AFC_SOURCE_MEDIA if !bundle.is_empty() => {
            bad("media AFC source must not carry a bundle id")
        }
        AFC_SOURCE_APP_DOCUMENTS if bundle.is_empty() => {
            bad("app Documents source requires a bundle id")
        }
        AFC_SOURCE_MEDIA => Ok(Source::Media),
        AFC_SOURCE_APP_DOCUMENTS => Ok(Source::AppDocuments(bundle)),
        _ => bad("bad AFC source"),
    }
}

/// An idle pooled client when the device is still on the same preferred
/// transport, otherwise a fresh connection. A vanished device loses its idle
/// clients and fails as DeviceNotFound.
async fn open_client(
    context: &EngineContext,
    key: &PoolKey,
) -> Result<(AfcClient, ClientOrigin), IdeviceError> {
    let device = devices_deduped(context)
        .await?
        .into_iter()
        .find(|device| device.udid == key.udid);
    let Some(device) = device else {
        context.afc_pool.forget(&key.udid);
        return Err(IdeviceError::DeviceNotFound);
    };
    let transport = Transport::from(&device.connection_type);
    let connect = connect(context, &device, &key.source);
    context.afc_pool.checkout(key, transport, connect).await
}

async fn connect(
    context: &EngineContext,
    device: &UsbmuxdDevice,
    source: &Source,
) -> Result<AfcClient, IdeviceError> {
    let provider = provider_from(context, device);
    match source {
        Source::Media => AfcClient::connect(&provider).await,
        Source::AppDocuments(bundle) => {
            let house_arrest = HouseArrestClient::connect(&provider).await?;
            house_arrest.vend_documents(bundle.clone()).await
        }
    }
}

/// Returns a closed handle's idle connection to the pool. A file first closes
/// its descriptor; if that fails the guard drops and closes the transport.
async fn release_to_pool(resource: Resource, origin: ClientOrigin) {
    let client = match resource {
        Resource::Session(client) => client,
        Resource::File(file) => {
            match tokio::time::timeout(timeouts::TEARDOWN, file.close()).await {
                Ok(Ok(client)) => client,
                _ => return,
            }
        }
    };
    origin.check_in(client);
}

#[derive(serde::Serialize)]
#[serde(rename_all = "camelCase")]
struct RawStat {
    is_dir: bool,
    size: u64,
    modified: i64,
}

/// Opens an AFC session, reusing an idle pooled connection while it is healthy.
/// rc 0 or an AV_ERROR_* kind.
#[no_mangle]
pub extern "C" fn av_afc_open(
    engine: *mut AvEngine,
    udid_ptr: *const u8,
    udid_len: usize,
    source: i32,
    bundle_ptr: *const u8,
    bundle_len: usize,
    out_session: *mut *mut AvAfcSession,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let context = unsafe { engine_ref(engine) }?.context();
        if out_session.is_null() {
            return Err(EngineFailure::invalid_argument("bad AFC session output"));
        }
        unsafe { *out_session = ptr::null_mut() };
        let udid = unsafe { input(udid_ptr, udid_len, MAX_UDID_BYTES, "udid", false) }?;
        let bundle = unsafe { input(bundle_ptr, bundle_len, MAX_BUNDLE_BYTES, "bundle id", true) }?;
        let key = PoolKey {
            source: parse_source(source, bundle)?,
            udid,
        };
        let (client, origin) = block_bounded(
            timeouts::DEVICE_WORK,
            "opening AFC session timed out",
            open_client(context, &key),
        )?;
        unsafe { *out_session = Box::into_raw(Box::new(AvAfcSession::new(client, origin))) };
        Ok(())
    })
}

/// Lists raw child names using an existing AFC connection. rc 0 or an
/// AV_ERROR_* kind (cancelled = the session was closed/cancelled).
#[no_mangle]
pub extern "C" fn av_afc_list(
    session: *mut AvAfcSession,
    path_ptr: *const u8,
    path_len: usize,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let slot = unsafe { session_slot(session) }?;
        reset_out(out_json, "bad AFC list output")?;
        let path = unsafe { physical_path(path_ptr, path_len) }?;
        let json =
            slot.run_session("listing AFC directory timed out", |mut client| async move {
                let result = client.list_dir(path).await.map(|names| to_json(&names));
                (Some(client), result)
            })?;
        out_str(out_json, &json);
        Ok(())
    })
}

/// Reads raw metadata using an existing AFC connection. rc 0 or an AV_ERROR_* kind.
#[no_mangle]
pub extern "C" fn av_afc_stat(
    session: *mut AvAfcSession,
    path_ptr: *const u8,
    path_len: usize,
    out_json: *mut *mut c_char,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let slot = unsafe { session_slot(session) }?;
        reset_out(out_json, "bad AFC stat output")?;
        let path = unsafe { physical_path(path_ptr, path_len) }?;
        let json = slot.run_session("AFC stat timed out", |mut client| async move {
            let result = client.get_file_info(path).await.map(|info| {
                to_json(&RawStat {
                    is_dir: info.st_ifmt == "S_IFDIR",
                    size: info.size as u64,
                    modified: info.modified.and_utc().timestamp(),
                })
            });
            (Some(client), result)
        })?;
        out_str(out_json, &json);
        Ok(())
    })
}

/// Removes one path using an existing AFC connection. rc 0 or an AV_ERROR_* kind.
#[no_mangle]
pub extern "C" fn av_afc_remove(
    session: *mut AvAfcSession,
    path_ptr: *const u8,
    path_len: usize,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let slot = unsafe { session_slot(session) }?;
        let path = unsafe { physical_path(path_ptr, path_len) }?;
        slot.run_session("removing AFC path timed out", |mut client| async move {
            let result = client.remove(path).await;
            (Some(client), result)
        })
    })
}

/// Stat one path and reject a directory.
async fn regular_file_info(client: &mut AfcClient, path: &str) -> Result<FileInfo, IdeviceError> {
    let info = client.get_file_info(path).await?;
    if info.st_ifmt == "S_IFDIR" {
        return Err(IdeviceError::Afc(AfcError::ObjectIsDir));
    }
    Ok(info)
}

/// Reads one whole file of at most `cap` bytes and hands the client back. A
/// refusal before the open (missing, a directory, too large) returns it with the
/// error; a transport failure, or any failure past open_owned, loses it.
pub(crate) async fn read_small_file(
    mut client: AfcClient,
    path: &str,
    cap: usize,
) -> Result<(AfcClient, Vec<u8>), (Option<AfcClient>, EngineFailure)> {
    let size = match regular_file_info(&mut client, path).await {
        Ok(info) if info.size > cap => {
            let failure = EngineFailure::new(
                ErrorKind::Internal,
                "AFC file is larger than the read buffer",
            );
            return Err((Some(client), failure));
        }
        Ok(info) => info.size,
        // An AFC status reply proves the connection still works.
        Err(error @ IdeviceError::Afc(_)) => return Err((Some(client), error.into())),
        Err(error) => return Err((None, error.into())),
    };
    let read = async {
        let mut file = FileGuard::new(client.open_owned(path, AfcFopenMode::RdOnly).await?);
        let bytes = file.read_n(size).await?;
        Ok((file.close().await?, bytes))
    };
    read.await
        .map_err(|error: IdeviceError| (None, error.into()))
}

/// Reads one whole small file (bounded by buffer_len) on an existing session
/// WITHOUT consuming it — the bulk path for thumbnails. Large streaming stays on
/// file_open/file_read. rc 0 or an AV_ERROR_* kind.
#[no_mangle]
pub extern "C" fn av_afc_read_small(
    session: *mut AvAfcSession,
    path_ptr: *const u8,
    path_len: usize,
    buffer: *mut u8,
    buffer_len: usize,
    out_read: *mut usize,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let slot = unsafe { session_slot(session) }?;
        reset_read(buffer, buffer_len, out_read)?;
        let path = unsafe { physical_path(path_ptr, path_len) }?;
        // A lost client closes the slot, so Go sees the next call as a dead session.
        let bytes = slot.run_session("reading AFC file timed out", |client| async move {
            match read_small_file(client, &path, buffer_len).await {
                Ok((client, bytes)) => (Some(client), Ok(bytes)),
                Err((client, failure)) => (client, Err(failure)),
            }
        })?;
        if bytes.len() > buffer_len {
            return Err(EngineFailure::new(
                ErrorKind::Internal,
                "AFC returned more data than requested",
            ));
        }
        unsafe { copy_read(&bytes, buffer, out_read) };
        Ok(())
    })
}

/// Transitions a session handle into one file for chunked reads, reporting its
/// size and modified time (unix seconds, 0 if unknown). rc 0 or an AV_ERROR_* kind.
#[no_mangle]
pub extern "C" fn av_afc_file_open(
    session: *mut AvAfcSession,
    path_ptr: *const u8,
    path_len: usize,
    out_size: *mut u64,
    out_modified: *mut i64,
    out_file: *mut *mut AvAfcFile,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let slot = unsafe { session_slot(session) }?;
        if out_size.is_null() || out_modified.is_null() || out_file.is_null() {
            return Err(EngineFailure::invalid_argument("bad AFC file output"));
        }
        unsafe {
            *out_size = 0;
            *out_modified = 0;
            *out_file = ptr::null_mut();
        }
        let path = unsafe { physical_path(path_ptr, path_len) }?;
        let mut client = slot.take_session()?;
        let info = slot.run(
            "AFC session cancelled",
            "opening AFC file timed out",
            async move {
                let opened = async {
                    let info = regular_file_info(&mut client, &path).await?;
                    let file = client.open_owned(path, AfcFopenMode::RdOnly).await?;
                    Ok::<_, IdeviceError>((FileGuard::new(file), info))
                };
                match opened.await {
                    Ok((file, info)) => (Some(Resource::File(file)), Ok(info)),
                    Err(error) => (None, Err(error.into())),
                }
            },
        )?;
        // Transfer the resource before publishing the file pointer: Go
        // releases the emptied session wrapper only after this returns.
        // SAFETY: session_slot above proved the handle non-null.
        let session = unsafe { &*session };
        if !session.detach(&slot) {
            drop(slot.close());
            return Err(cancelled("AFC session cancelled"));
        }
        let origin = session.origin.clone();
        unsafe {
            *out_size = info.size as u64;
            *out_modified = info.modified.and_utc().timestamp();
            *out_file = Box::into_raw(Box::new(AvAfcFile { slot, origin }));
        }
        Ok(())
    })
}

/// Reads at most buffer_len bytes into the caller's buffer. rc 0 or an
/// AV_ERROR_* kind.
#[no_mangle]
pub extern "C" fn av_afc_file_read(
    file: *mut AvAfcFile,
    buffer: *mut u8,
    buffer_len: usize,
    out_read: *mut usize,
    error: *mut AvError,
) -> i32 {
    guard_error(error, || {
        let Some(file) = (unsafe { file.as_ref() }) else {
            return Err(cancelled("AFC file is closed"));
        };
        let slot = &file.slot;
        reset_read(buffer, buffer_len, out_read)?;
        let mut guard = slot.take_file()?;
        let bytes = slot.run(
            "AFC file cancelled",
            "reading AFC file timed out",
            async move {
                match guard.read_n(buffer_len).await {
                    // A well-behaved device never returns more than requested; refuse an
                    // oversized frame instead of overflowing the caller's fixed buffer.
                    Ok(bytes) if bytes.len() > buffer_len => (
                        None,
                        Err(EngineFailure::new(
                            ErrorKind::Internal,
                            "AFC returned more data than requested",
                        )),
                    ),
                    Ok(bytes) => (Some(Resource::File(guard)), Ok(bytes)),
                    // AFC represents a normal read past the end as a status error.
                    // Normalize that protocol detail at the native boundary so Go
                    // receives the ordinary io.Reader contract (zero bytes = EOF).
                    Err(IdeviceError::Afc(AfcError::EndOfData)) => {
                        (Some(Resource::File(guard)), Ok(Vec::new()))
                    }
                    Err(error) => (None, Err(error.into())),
                }
            },
        )?;
        unsafe { copy_read(&bytes, buffer, out_read) };
        Ok(())
    })
}

/// Moves the read cursor to an absolute offset. rc 0 or an AV_ERROR_* kind.
#[no_mangle]
pub extern "C" fn av_afc_file_seek(file: *mut AvAfcFile, offset: u64, error: *mut AvError) -> i32 {
    guard_error(error, || {
        let Some(file) = (unsafe { file.as_ref() }) else {
            return Err(cancelled("AFC file is closed"));
        };
        let slot = &file.slot;
        let mut guard = slot.take_file()?;
        slot.run(
            "AFC file cancelled",
            "seeking AFC file timed out",
            async move {
                match guard.seek(std::io::SeekFrom::Start(offset)).await {
                    Ok(_) => (Some(Resource::File(guard)), Ok(())),
                    Err(error) => (None, Err(IdeviceError::from(error).into())),
                }
            },
        )
    })
}

/// Interrupts an in-flight call without releasing the wrapper; the interrupted
/// connection is dropped, never pooled. Go calls this before waiting for the
/// in-flight FFI operation, then calls close after it returns.
#[no_mangle]
pub unsafe extern "C" fn av_afc_cancel(session: *mut AvAfcSession) {
    if let Ok(slot) = unsafe { session_slot(session) } {
        slot.cancel();
    }
}

#[no_mangle]
pub unsafe extern "C" fn av_afc_file_cancel(file: *mut AvAfcFile) {
    if let Some(file) = unsafe { file.as_ref() } {
        file.slot.cancel();
    }
}

/// Closes an AFC session and returns its idle connection to the pool. A null
/// pointer is a no-op.
#[no_mangle]
pub unsafe extern "C" fn av_afc_close(session: *mut AvAfcSession) {
    if !session.is_null() {
        let session = unsafe { Box::from_raw(session) };
        let slot = crate::lock(&session.slot).take();
        if let Some(resource) = slot.and_then(|slot| slot.close()) {
            block(release_to_pool(resource, session.origin));
        }
    }
}

/// Closes an AFC file (one FileClose round trip) and returns its connection to
/// the pool. A null pointer is a no-op.
#[no_mangle]
pub unsafe extern "C" fn av_afc_file_close(file: *mut AvAfcFile) {
    if !file.is_null() {
        let file = unsafe { Box::from_raw(file) };
        if let Some(resource) = file.slot.close() {
            block(release_to_pool(resource, file.origin));
        }
    }
}

#[cfg(test)]
#[path = "afc_lifecycle_tests.rs"]
pub(crate) mod lifecycle_tests;

#[cfg(test)]
mod tests {
    use std::path::PathBuf;
    use std::ptr;

    use super::{av_afc_open, physical_path, AvAfcSession, ErrorKind, Slot};
    use crate::ffi::{av_buffer_free, AvBuffer, AvEngine, AvError};
    use crate::provider::EngineContext;

    #[test]
    fn physical_paths_require_absolute_utf8() {
        let phys = |s: &str| unsafe { physical_path(s.as_ptr(), s.len()) };
        assert_eq!(
            phys("/DCIM/100APPLE/A file.HEIC").unwrap(),
            "/DCIM/100APPLE/A file.HEIC"
        );
        assert!(phys("DCIM/file").is_err());
        assert!(phys("").is_err());
        // . and .. are devicefs policy; the transport passes them through.
        assert_eq!(phys("/DCIM/../file").unwrap(), "/DCIM/../file");
    }

    #[test]
    fn open_reports_a_bad_source_through_rc_and_detail() {
        let udid = b"test-udid";
        let context = EngineContext::new(
            PathBuf::from("/tmp/airvault-backups"),
            PathBuf::from("/tmp/airvault-pairing"),
            None,
        )
        .unwrap();
        let mut engine = Box::new(AvEngine::new(context));
        let mut session: *mut AvAfcSession = ptr::null_mut();
        let mut error = AvError {
            detail: AvBuffer {
                ptr: ptr::null_mut(),
                len: 0,
            },
        };
        let rc = av_afc_open(
            engine.as_mut(),
            udid.as_ptr(),
            udid.len(),
            99,
            ptr::null(),
            0,
            &mut session,
            &mut error,
        );

        assert_eq!(rc, ErrorKind::InvalidArgument.code());
        assert!(session.is_null());
        assert!(!error.detail.ptr.is_null());
        let detail = unsafe { std::slice::from_raw_parts(error.detail.ptr, error.detail.len) };
        assert_eq!(detail, b"bad AFC source");
        unsafe { av_buffer_free(error.detail) };
    }

    // Cancel refuses further operations but leaves an idle resource for close
    // to hand back; a resource in flight at cancel can never be put back.
    #[test]
    fn cancel_drops_only_a_resource_in_flight() {
        let idle = Slot::new(7);
        idle.cancel();
        assert_eq!(idle.take(), None);
        assert_eq!(idle.close(), Some(7));

        let in_flight = Slot::new(7);
        assert_eq!(in_flight.take(), Some(7));
        in_flight.cancel();
        assert_eq!(in_flight.put(7), Err(7));
        assert_eq!(in_flight.close(), None);
    }
}
