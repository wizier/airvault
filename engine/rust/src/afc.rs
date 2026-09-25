//! Stateful Apple File Conduit transport. Go owns paths, pagination, copying
//! and progress; this module owns only AFC connections and cancellable reads.

use std::ffi::c_char;
use std::future::Future;
use std::ops::{Deref, DerefMut};
use std::ptr;
use std::sync::{Arc, Mutex};

use idevice::services::afc::errors::AfcError;
use idevice::services::afc::file::OwnedFileDescriptor;
use idevice::services::afc::{
    opcode::{AfcFopenMode, AfcOpcode},
    AfcClient,
};
use idevice::services::house_arrest::HouseArrestClient;
use idevice::{IdeviceError, IdeviceService};
use tokio_util::sync::CancellationToken;

use crate::bounded::{cancel_or_timeout, Interrupt};
use crate::engine_error::{EngineFailure, ErrorKind};
use crate::ffi::{engine_ref, guard_error, AvEngine, AvError};
use crate::timeouts;
use crate::{
    block, block_bounded, out_str, provider_for, to_json, write_failure, EngineContext,
    MAX_UDID_BYTES,
};

const AFC_SOURCE_MEDIA: i32 = 0;
const AFC_SOURCE_APP_DOCUMENTS: i32 = 1;
const MAX_BUNDLE_BYTES: usize = 512;
const MAX_PATH_BYTES: usize = 4096;
const MAX_READ_BYTES: usize = 1024 * 1024;

struct SlotState<T> {
    resource: Option<T>,
    closed: bool,
}

// One lock is the linearization point for operation completion and Close.
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

    fn close(&self) -> Option<T> {
        let resource = {
            let mut state = crate::lock(&self.state);
            state.closed = true;
            state.resource.take()
        };
        self.cancel.cancel();
        resource
    }
}

enum Resource {
    Session(AfcClient),
    File(FileGuard),
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
/// operation (or vice versa).
pub struct AvAfcSession {
    // File-open moves the slot to AvAfcFile while this wrapper stays alive for
    // the rest of the FFI call, so a concurrent cancel cannot free it early.
    slot: Mutex<Option<Arc<Slot<Resource>>>>,
}

pub struct AvAfcFile {
    slot: Arc<Slot<Resource>>,
}

impl AvAfcSession {
    fn new(slot: Arc<Slot<Resource>>) -> Self {
        Self {
            slot: Mutex::new(Some(slot)),
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

    fn close(&self) {
        if let Some(slot) = crate::lock(&self.slot).take() {
            close_slot(&slot);
        }
    }
}

fn fail(err: *mut *mut c_char, kind: ErrorKind, message: &str) -> i32 {
    write_failure(err, EngineFailure::new(kind, message))
}

unsafe fn input(
    ptr: *const u8,
    len: usize,
    max: usize,
    label: &str,
    allow_empty: bool,
) -> Result<String, String> {
    if len > max || (ptr.is_null() && len != 0) {
        return Err(format!("bad {label}"));
    }
    if len == 0 {
        return if allow_empty {
            Ok(String::new())
        } else {
            Err(format!("bad {label}"))
        };
    }
    // SAFETY: the caller promises a readable buffer for the duration of this
    // synchronous FFI call; length was bounded above.
    let bytes = unsafe { std::slice::from_raw_parts(ptr, len) };
    if bytes.contains(&0) {
        return Err(format!("bad {label}: contains NUL"));
    }
    std::str::from_utf8(bytes)
        .map(str::to_owned)
        .map_err(|_| format!("bad {label}: not UTF-8"))
}

// Go (devicefs) owns path policy — component structure and . / .. rejection.
// Here only the raw-pointer memory safety in input() plus an absolute-path
// shape check remain; the AFC OS jail confines every path regardless.
unsafe fn physical_path(ptr: *const u8, len: usize) -> Result<String, String> {
    let path = unsafe { input(ptr, len, MAX_PATH_BYTES, "AFC path", false) }?;
    if !path.starts_with('/') {
        return Err("bad AFC path".into());
    }
    Ok(path)
}

enum Source {
    Media,
    AppDocuments,
}

fn parse_source(source: i32, bundle: &str) -> Result<Source, String> {
    match source {
        AFC_SOURCE_MEDIA if !bundle.is_empty() => {
            Err("media AFC source must not carry a bundle id".into())
        }
        AFC_SOURCE_APP_DOCUMENTS if bundle.is_empty() => {
            Err("app Documents source requires a bundle id".into())
        }
        AFC_SOURCE_MEDIA => Ok(Source::Media),
        AFC_SOURCE_APP_DOCUMENTS => Ok(Source::AppDocuments),
        _ => Err("bad AFC source".into()),
    }
}

async fn connect(
    context: &EngineContext,
    udid: &str,
    source: Source,
    bundle: &str,
) -> Result<AfcClient, IdeviceError> {
    let provider = provider_for(context, udid).await?;
    match source {
        Source::Media => AfcClient::connect(&provider).await,
        Source::AppDocuments => {
            let house_arrest = HouseArrestClient::connect(&provider).await?;
            house_arrest.vend_documents(bundle.to_owned()).await
        }
    }
}

fn restore(slot: &Slot<Resource>, resource: Resource) {
    drop(slot.put(resource));
}

fn close_slot(slot: &Slot<Resource>) {
    drop(slot.close());
}

/// Dispose an in-hand file (closing its dedicated AFC session) and tear down its
/// handle — the failure teardown after a file has been taken out of its slot.
fn discard_file(slot: &Slot<Resource>, file: FileGuard) {
    drop(file);
    close_slot(slot);
}

#[derive(serde::Serialize)]
#[serde(rename_all = "camelCase")]
struct RawStat {
    is_dir: bool,
    size: u64,
    modified: i64,
}

// The operation returns the client for reuse, or None when the transport is
// lost with the failure and the slot must close.
fn run_session<T, E, F, Fut>(
    slot: &Arc<Slot<Resource>>,
    timeout_message: &'static str,
    operation: F,
) -> Result<T, EngineFailure>
where
    E: Into<EngineFailure>,
    F: FnOnce(AfcClient) -> Fut,
    Fut: Future<Output = (Option<AfcClient>, Result<T, E>)>,
{
    let client = match slot.take() {
        Some(Resource::Session(client)) => client,
        Some(resource) => {
            restore(slot, resource);
            return Err(EngineFailure::new(
                ErrorKind::Cancelled,
                "AFC handle is not a session",
            ));
        }
        None => {
            return Err(EngineFailure::new(
                ErrorKind::Cancelled,
                "AFC session is closed",
            ))
        }
    };
    let outcome = block(cancel_or_timeout(
        &slot.cancel,
        timeouts::DEVICE_WORK,
        operation(client),
    ));
    match outcome {
        Ok((Some(client), result)) => {
            if slot.put(Resource::Session(client)).is_err() {
                return Err(EngineFailure::new(
                    ErrorKind::Cancelled,
                    "AFC session cancelled",
                ));
            }
            result.map_err(Into::into)
        }
        Ok((None, result)) => {
            close_slot(slot);
            result.map_err(Into::into)
        }
        Err(Interrupt::Cancelled) => {
            close_slot(slot);
            Err(EngineFailure::new(
                ErrorKind::Cancelled,
                "AFC session cancelled",
            ))
        }
        Err(Interrupt::TimedOut) => {
            close_slot(slot);
            Err(EngineFailure::new(ErrorKind::Timeout, timeout_message))
        }
    }
}

/// Opens one short-lived AFC connection. rc 0 or an AV_ERROR_* kind.
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
    guard_error(error, |err| {
        let Some(engine) = (unsafe { engine_ref(engine, err) }) else {
            return ErrorKind::InvalidArgument.code();
        };
        let context = engine.context();
        if out_session.is_null() {
            return fail(err, ErrorKind::InvalidArgument, "bad AFC session output");
        }
        unsafe { *out_session = ptr::null_mut() };
        let udid = match unsafe { input(udid_ptr, udid_len, MAX_UDID_BYTES, "udid", false) } {
            Ok(value) => value,
            Err(message) => return fail(err, ErrorKind::InvalidArgument, &message),
        };
        let bundle =
            match unsafe { input(bundle_ptr, bundle_len, MAX_BUNDLE_BYTES, "bundle id", true) } {
                Ok(value) => value,
                Err(message) => return fail(err, ErrorKind::InvalidArgument, &message),
            };
        let source = match parse_source(source, &bundle) {
            Ok(value) => value,
            Err(message) => return fail(err, ErrorKind::InvalidArgument, &message),
        };
        block_bounded(
            err,
            timeouts::DEVICE_WORK,
            "opening AFC session timed out",
            connect(context, &udid, source, &bundle),
            |client| {
                let slot = Arc::new(Slot::new(Resource::Session(client)));
                unsafe { *out_session = Box::into_raw(Box::new(AvAfcSession::new(slot))) };
                0
            },
        )
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
    guard_error(error, |err| {
        let Some(session) = (unsafe { session.as_ref() }) else {
            return fail(err, ErrorKind::Cancelled, "AFC session is closed");
        };
        let Some(slot) = session.slot() else {
            return fail(err, ErrorKind::Cancelled, "AFC session is closed");
        };
        if out_json.is_null() {
            return fail(err, ErrorKind::InvalidArgument, "bad AFC list output");
        }
        unsafe { *out_json = ptr::null_mut() };
        let path = match unsafe { physical_path(path_ptr, path_len) } {
            Ok(value) => value,
            Err(message) => return fail(err, ErrorKind::InvalidArgument, &message),
        };
        let result = run_session(
            &slot,
            "listing AFC directory timed out",
            |mut client| async move {
                let result = client.list_dir(path).await.map(|names| to_json(&names));
                (Some(client), result)
            },
        );
        match result {
            Ok(json) => {
                out_str(out_json, &json);
                0
            }
            Err(failure) => write_failure(err, failure),
        }
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
    guard_error(error, |err| {
        let Some(session) = (unsafe { session.as_ref() }) else {
            return fail(err, ErrorKind::Cancelled, "AFC session is closed");
        };
        let Some(slot) = session.slot() else {
            return fail(err, ErrorKind::Cancelled, "AFC session is closed");
        };
        if out_json.is_null() {
            return fail(err, ErrorKind::InvalidArgument, "bad AFC stat output");
        }
        unsafe { *out_json = ptr::null_mut() };
        let path = match unsafe { physical_path(path_ptr, path_len) } {
            Ok(value) => value,
            Err(message) => return fail(err, ErrorKind::InvalidArgument, &message),
        };
        let result = run_session(&slot, "AFC stat timed out", |mut client| async move {
            let result = client.get_file_info(path).await.map(|info| {
                to_json(&RawStat {
                    is_dir: info.st_ifmt == "S_IFDIR",
                    size: info.size as u64,
                    modified: info.modified.and_utc().timestamp(),
                })
            });
            (Some(client), result)
        });
        match result {
            Ok(json) => {
                out_str(out_json, &json);
                0
            }
            Err(failure) => write_failure(err, failure),
        }
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
    guard_error(error, |err| {
        let Some(session) = (unsafe { session.as_ref() }) else {
            return fail(err, ErrorKind::Cancelled, "AFC session is closed");
        };
        let Some(slot) = session.slot() else {
            return fail(err, ErrorKind::Cancelled, "AFC session is closed");
        };
        let path = match unsafe { physical_path(path_ptr, path_len) } {
            Ok(value) => value,
            Err(message) => return fail(err, ErrorKind::InvalidArgument, &message),
        };
        let result = run_session(
            &slot,
            "removing AFC path timed out",
            |mut client| async move {
                let result = client.remove(path).await;
                (Some(client), result)
            },
        );
        match result {
            Ok(()) => 0,
            Err(failure) => write_failure(err, failure),
        }
    })
}

/// Stat one path and reject a directory, returning the regular file's size.
async fn regular_file_size(client: &mut AfcClient, path: &str) -> Result<usize, IdeviceError> {
    let info = client.get_file_info(path).await?;
    if info.st_ifmt == "S_IFDIR" {
        return Err(IdeviceError::Afc(AfcError::ObjectIsDir));
    }
    Ok(info.size)
}

/// Reads one whole file of at most `cap` bytes and hands the client back. A
/// refusal before the open (missing, a directory, too large) returns it with the
/// error; a transport failure, or any failure past open_owned, loses it.
pub(crate) async fn read_small_file(
    mut client: AfcClient,
    path: &str,
    cap: usize,
) -> Result<(AfcClient, Vec<u8>), (Option<AfcClient>, EngineFailure)> {
    let size = match regular_file_size(&mut client, path).await {
        Ok(size) if size > cap => {
            let failure = EngineFailure::new(
                ErrorKind::Internal,
                "AFC file is larger than the read buffer",
            );
            return Err((Some(client), failure));
        }
        Ok(size) => size,
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
    guard_error(error, |err| {
        let Some(session) = (unsafe { session.as_ref() }) else {
            return fail(err, ErrorKind::Cancelled, "AFC session is closed");
        };
        let Some(slot) = session.slot() else {
            return fail(err, ErrorKind::Cancelled, "AFC session is closed");
        };
        if buffer.is_null() || out_read.is_null() || buffer_len == 0 || buffer_len > MAX_READ_BYTES
        {
            return fail(err, ErrorKind::InvalidArgument, "bad AFC read buffer");
        }
        unsafe { *out_read = 0 };
        let path = match unsafe { physical_path(path_ptr, path_len) } {
            Ok(value) => value,
            Err(message) => return fail(err, ErrorKind::InvalidArgument, &message),
        };
        // A lost client closes the slot, so Go sees the next call as a dead session.
        let result = run_session(&slot, "reading AFC file timed out", |client| async move {
            match read_small_file(client, &path, buffer_len).await {
                Ok((client, bytes)) => (Some(client), Ok(bytes)),
                Err((client, failure)) => (client, Err(failure)),
            }
        });
        match result {
            Ok(bytes) => {
                if bytes.len() > buffer_len {
                    return fail(
                        err,
                        ErrorKind::Internal,
                        "AFC returned more data than requested",
                    );
                }
                unsafe {
                    ptr::copy_nonoverlapping(bytes.as_ptr(), buffer, bytes.len());
                    *out_read = bytes.len();
                }
                0
            }
            Err(failure) => write_failure(err, failure),
        }
    })
}

/// Transitions a session handle into one file for chunked reads. rc 0 or an
/// AV_ERROR_* kind.
#[no_mangle]
pub extern "C" fn av_afc_file_open(
    session: *mut AvAfcSession,
    path_ptr: *const u8,
    path_len: usize,
    out_size: *mut u64,
    out_file: *mut *mut AvAfcFile,
    error: *mut AvError,
) -> i32 {
    guard_error(error, |err| {
        let Some(session_ref) = (unsafe { session.as_ref() }) else {
            return fail(err, ErrorKind::Cancelled, "AFC session is closed");
        };
        let Some(slot) = session_ref.slot() else {
            return fail(err, ErrorKind::Cancelled, "AFC session is closed");
        };
        if out_size.is_null() || out_file.is_null() {
            return fail(err, ErrorKind::InvalidArgument, "bad AFC file output");
        }
        unsafe {
            *out_size = 0;
            *out_file = ptr::null_mut();
        }
        let path = match unsafe { physical_path(path_ptr, path_len) } {
            Ok(value) => value,
            Err(message) => return fail(err, ErrorKind::InvalidArgument, &message),
        };
        let mut client = match slot.take() {
            Some(Resource::Session(client)) => client,
            Some(resource) => {
                restore(&slot, resource);
                return fail(err, ErrorKind::Cancelled, "AFC handle is not a session");
            }
            None => return fail(err, ErrorKind::Cancelled, "AFC session is closed"),
        };
        let outcome = block(cancel_or_timeout(
            &slot.cancel,
            timeouts::DEVICE_WORK,
            async move {
                let size = regular_file_size(&mut client, &path).await? as u64;
                let file = client.open_owned(path, AfcFopenMode::RdOnly).await?;
                Ok::<_, IdeviceError>((FileGuard::new(file), size))
            },
        ));
        match outcome {
            Ok(Ok((file, size))) => {
                if slot.put(Resource::File(file)).is_err() {
                    return fail(err, ErrorKind::Cancelled, "AFC session cancelled");
                }
                // Transfer the resource before publishing the file pointer: Go
                // releases the emptied session wrapper only after this returns.
                if !session_ref.detach(&slot) {
                    close_slot(&slot);
                    return fail(err, ErrorKind::Cancelled, "AFC session cancelled");
                }
                unsafe {
                    *out_size = size;
                    *out_file = Box::into_raw(Box::new(AvAfcFile { slot }));
                }
                0
            }
            Ok(Err(error)) => {
                close_slot(&slot);
                write_failure(err, error.into())
            }
            Err(Interrupt::Cancelled) => {
                close_slot(&slot);
                fail(err, ErrorKind::Cancelled, "AFC session cancelled")
            }
            Err(Interrupt::TimedOut) => {
                close_slot(&slot);
                fail(err, ErrorKind::Timeout, "opening AFC file timed out")
            }
        }
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
    guard_error(error, |err| {
        let Some(file_ref) = (unsafe { file.as_ref() }) else {
            return fail(err, ErrorKind::Cancelled, "AFC file is closed");
        };
        let slot = &file_ref.slot;
        if buffer.is_null() || out_read.is_null() || buffer_len == 0 || buffer_len > MAX_READ_BYTES
        {
            return fail(err, ErrorKind::InvalidArgument, "bad AFC read buffer");
        }
        unsafe { *out_read = 0 };
        let mut file = match slot.take() {
            Some(Resource::File(file)) => file,
            Some(resource) => {
                restore(slot, resource);
                return fail(err, ErrorKind::Cancelled, "AFC handle is not a file");
            }
            None => return fail(err, ErrorKind::Cancelled, "AFC file is closed"),
        };
        let outcome = block(cancel_or_timeout(
            &slot.cancel,
            timeouts::DEVICE_WORK,
            async {
                match file.read_n(buffer_len).await {
                    // AFC represents a normal read past the end as a status error.
                    // Normalize that protocol detail at the native boundary so Go
                    // receives the ordinary io.Reader contract (zero bytes = EOF).
                    Err(IdeviceError::Afc(AfcError::EndOfData)) => Ok(Vec::new()),
                    result => result,
                }
            },
        ));
        match outcome {
            Ok(Ok(bytes)) => {
                // A well-behaved device never returns more than requested; refuse an
                // oversized frame instead of overflowing the caller's fixed buffer.
                if bytes.len() > buffer_len {
                    discard_file(slot, file);
                    return fail(
                        err,
                        ErrorKind::Internal,
                        "AFC returned more data than requested",
                    );
                }
                if slot.put(Resource::File(file)).is_err() {
                    return fail(err, ErrorKind::Cancelled, "AFC file cancelled");
                }
                unsafe {
                    ptr::copy_nonoverlapping(bytes.as_ptr(), buffer, bytes.len());
                    *out_read = bytes.len();
                }
                0
            }
            Ok(Err(error)) => {
                discard_file(slot, file);
                write_failure(err, error.into())
            }
            Err(Interrupt::Cancelled) => {
                discard_file(slot, file);
                fail(err, ErrorKind::Cancelled, "AFC file cancelled")
            }
            Err(Interrupt::TimedOut) => {
                discard_file(slot, file);
                fail(err, ErrorKind::Timeout, "reading AFC file timed out")
            }
        }
    })
}

/// Signals cancellation without releasing the wrapper. Go calls this before
/// waiting for an in-flight FFI operation, then calls close after it returns.
#[no_mangle]
pub unsafe extern "C" fn av_afc_cancel(session: *mut AvAfcSession) {
    if let Some(session) = unsafe { session.as_ref() } {
        if let Some(slot) = session.slot() {
            close_slot(&slot);
        }
    }
}

#[no_mangle]
pub unsafe extern "C" fn av_afc_file_cancel(file: *mut AvAfcFile) {
    if let Some(file) = unsafe { file.as_ref() } {
        close_slot(&file.slot);
    }
}

/// Cancels and closes an AFC session. A null pointer is a no-op.
#[no_mangle]
pub unsafe extern "C" fn av_afc_close(session: *mut AvAfcSession) {
    if !session.is_null() {
        let session = unsafe { Box::from_raw(session) };
        session.close();
    }
}

/// Cancels and closes an AFC file. A null pointer is a no-op.
#[no_mangle]
pub unsafe extern "C" fn av_afc_file_close(file: *mut AvAfcFile) {
    if !file.is_null() {
        let file = unsafe { Box::from_raw(file) };
        close_slot(&file.slot);
    }
}

#[cfg(test)]
#[path = "afc_lifecycle_tests.rs"]
mod lifecycle_tests;

#[cfg(test)]
mod tests {
    use std::path::PathBuf;
    use std::ptr;

    use super::{av_afc_open, physical_path, AvAfcSession, ErrorKind, Slot};
    use crate::ffi::{av_buffer_free, AvBuffer, AvEngine, AvError};
    use crate::EngineContext;

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
    fn session_open_enters_runtime_before_constructing_timer() {
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

    #[test]
    fn closed_slot_rejects_in_flight_resource() {
        let slot = Slot::new(7);
        assert_eq!(slot.take(), Some(7));
        assert_eq!(slot.close(), None);
        assert_eq!(slot.put(7), Err(7));
    }

    #[test]
    fn close_collects_idle_resource() {
        let slot = Slot::new(7);
        assert_eq!(slot.close(), Some(7));
        assert_eq!(slot.take(), None);
    }
}
