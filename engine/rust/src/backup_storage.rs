//! The single mobilebackup2 BackupDelegate: sandboxes device paths into
//! logical store keys, reports transfer progress, and records the first
//! sandbox violation, delegating storage to the session's object store. The
//! only place store and sandbox outcomes become idevice protocol types.

use std::future::Future;
use std::io::{Read, Write};
use std::path::Path;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex as StdMutex;
use std::time::{Duration, UNIX_EPOCH};

use idevice::services::mobilebackup2::{
    BackupDelegate, BackupProgress, DirEntryInfo, FsBackupDelegate,
};
use idevice::IdeviceError;

use crate::object_store::ObjectSession;
use crate::path_sandbox::PathSandbox;

/// Progress callback into Go: (opaque operation id, phase, percent, bytes).
/// Phase is AV_BACKUP_PHASE_*; percent < 0 means "not reported this call", and
/// `bytes` is the session's cumulative total (0 = not reported this call).
pub(crate) type BackupCb = extern "C" fn(usize, i32, f64, u64);
pub const AV_BACKUP_PHASE_TRANSFER: i32 = 0;
pub const AV_BACKUP_PHASE_FINALIZING: i32 = 1;

/// Where progress frames go: the Go callback plus its operation id.
#[derive(Clone, Copy)]
pub(crate) struct ProgressSink {
    pub(crate) callback: BackupCb,
    pub(crate) id: usize,
}

impl ProgressSink {
    pub(crate) fn emit(self, phase: i32, percent: f64, bytes: u64) {
        (self.callback)(self.id, phase, percent, bytes);
    }
}

type DelegateFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// Progress reporting and a per-source sandbox around one object session.
pub(crate) struct BackupStorage {
    session: ObjectSession,
    sandbox: PathSandbox,
    violation: StdMutex<Option<String>>,
    progress: ProgressSink,
    started: AtomicBool,
}

impl BackupStorage {
    pub(crate) fn new(
        session: ObjectSession,
        sandbox: PathSandbox,
        progress: ProgressSink,
    ) -> Self {
        Self {
            session,
            sandbox,
            violation: StdMutex::new(None),
            progress,
            started: AtomicBool::new(false),
        }
    }

    /// The first storage op after the device request means its passcode gate
    /// has cleared and the DeviceLink loop has begun.
    fn touch(&self) {
        if !self.started.swap(true, Ordering::Relaxed) {
            // Nothing has moved yet, so the frame itself is the whole signal.
            self.progress.emit(AV_BACKUP_PHASE_TRANSFER, -1.0, 0);
        }
    }

    fn resolve_key(&self, path: &Path) -> Result<String, IdeviceError> {
        self.sandbox.resolve_key(path).map_err(|detail| {
            let error = IdeviceError::UnexpectedResponse(detail);
            crate::lock(&self.violation).get_or_insert_with(|| error.to_string());
            error
        })
    }

    fn resolve_key_pair(&self, from: &Path, to: &Path) -> Result<(String, String), IdeviceError> {
        Ok((self.resolve_key(from)?, self.resolve_key(to)?))
    }

    pub(crate) fn violation(&self) -> Option<String> {
        crate::lock(&self.violation).clone()
    }
}

// Every op below (except create_dir_all, which the crate calls before the
// backup request goes out) only happens inside the DL loop → touch().
// Store refusals carry only their detail to the device (InternalError).
impl BackupDelegate for BackupStorage {
    fn get_free_disk_space(&self, _path: &Path) -> u64 {
        self.touch();
        // Reuse only idevice's portable filesystem-capacity query. All backup
        // reads and writes remain object-store operations.
        FsBackupDelegate.get_free_disk_space(self.session.root())
    }
    fn open_file_read<'a>(
        &'a self,
        path: &'a Path,
    ) -> DelegateFuture<'a, Result<Box<dyn Read + Send>, IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let key = self.resolve_key(path)?;
            self.session
                .open_file_read(&key)
                .map_err(IdeviceError::InternalError)?
                .ok_or(IdeviceError::NotFound)
        })
    }
    fn create_file_write<'a>(
        &'a self,
        path: &'a Path,
    ) -> DelegateFuture<'a, Result<Box<dyn Write + Send>, IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let key = self.resolve_key(path)?;
            self.session
                .create_file_write(&key)
                .map_err(IdeviceError::InternalError)
        })
    }
    fn create_dir_all<'a>(
        &'a self,
        path: &'a Path,
    ) -> DelegateFuture<'a, Result<(), IdeviceError>> {
        Box::pin(async move {
            let key = self.resolve_key(path)?;
            self.session
                .create_dir_all(&key)
                .map_err(IdeviceError::InternalError)
        })
    }
    fn remove<'a>(&'a self, path: &'a Path) -> DelegateFuture<'a, Result<(), IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let key = self.resolve_key(path)?;
            self.session
                .remove(&key)
                .map_err(IdeviceError::InternalError)
        })
    }
    fn rename<'a>(
        &'a self,
        from: &'a Path,
        to: &'a Path,
    ) -> DelegateFuture<'a, Result<(), IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let (from, to) = self.resolve_key_pair(from, to)?;
            self.session
                .rename(&from, &to)
                .map_err(IdeviceError::InternalError)
        })
    }
    fn copy<'a>(
        &'a self,
        src: &'a Path,
        dst: &'a Path,
    ) -> DelegateFuture<'a, Result<(), IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let (src, dst) = self.resolve_key_pair(src, dst)?;
            self.session
                .copy(&src, &dst)
                .map_err(IdeviceError::InternalError)
        })
    }
    fn exists<'a>(&'a self, path: &'a Path) -> DelegateFuture<'a, bool> {
        self.touch();
        Box::pin(async move {
            self.resolve_key(path)
                .is_ok_and(|key| self.session.exists(&key))
        })
    }
    fn is_dir<'a>(&'a self, path: &'a Path) -> DelegateFuture<'a, bool> {
        self.touch();
        Box::pin(async move {
            self.resolve_key(path)
                .is_ok_and(|key| self.session.is_dir(&key))
        })
    }
    fn list_dir<'a>(
        &'a self,
        path: &'a Path,
    ) -> DelegateFuture<'a, Result<Vec<DirEntryInfo>, IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let key = self.resolve_key(path)?;
            let entries = self.session.list_dir(&key);
            let entries = entries.map_err(IdeviceError::InternalError)?.into_iter();
            Ok(entries
                .map(|entry| DirEntryInfo {
                    name: entry.name,
                    is_dir: entry.size.is_none(),
                    is_file: entry.size.is_some(),
                    size: entry.size.unwrap_or(0),
                    modified: (entry.modified_unix > 0)
                        .then(|| UNIX_EPOCH + Duration::from_secs(entry.modified_unix as u64)),
                })
                .collect())
        })
    }
    // Both payload directions, at least once per 256 KiB and mid-file, so this
    // is the whole byte account as well as the percentage.
    fn on_progress(&self, progress: BackupProgress) {
        self.started.store(true, Ordering::Relaxed); // real frame follows anyway
        self.progress.emit(
            AV_BACKUP_PHASE_TRANSFER,
            progress.overall_progress,
            progress.session_bytes_done,
        );
    }
}
