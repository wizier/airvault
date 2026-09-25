//! The single mobilebackup2 BackupDelegate: sandboxes device paths into
//! logical store keys, reports transfer progress, and records the first
//! sandbox violation, delegating storage to the session's object store.

use std::future::Future;
use std::path::Path;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Mutex as StdMutex;

use idevice::services::mobilebackup2::{
    BackupDelegate, BackupProgress as Mb2Progress, DirEntryInfo,
};
use std::io::{Read, Write};

use crate::object_store::ObjectSession;
use crate::path_sandbox::PathSandbox;

/// Progress callback into Go: (opaque operation id, phase, percent, bytes).
/// Phase is BACKUP_PHASE_*; percent < 0 means "not reported this call", and
/// `bytes` counts this call alone — the host owns the running sum.
pub(crate) type BackupCb = extern "C" fn(usize, i32, f64, u64);
pub(crate) const BACKUP_PHASE_TRANSFER: i32 = 0;
pub(crate) const BACKUP_PHASE_FINALIZING: i32 = 1;

#[derive(Clone, Copy)]
pub(crate) struct BackupProgress {
    pub(crate) callback: BackupCb,
    pub(crate) id: usize,
}

impl BackupProgress {
    pub(crate) fn emit(self, phase: i32, percent: f64, bytes: u64) {
        (self.callback)(self.id, phase, percent, bytes);
    }
}

/// The crate reports session-wide totals; the host contract wants what one
/// frame alone moved. `fetch_max` keeps a high-water mark, so a frame that
/// repeats or regresses contributes zero instead of double-counting later.
#[derive(Default)]
struct DeltaMeter(AtomicU64);

impl DeltaMeter {
    fn advance(&self, session_bytes_done: u64) -> u64 {
        let previous = self.0.fetch_max(session_bytes_done, Ordering::Relaxed);
        session_bytes_done.saturating_sub(previous)
    }
}

type DelegateFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// Progress reporting and a per-source sandbox around one object session.
pub(crate) struct BackupStorage {
    session: ObjectSession,
    sandbox: PathSandbox,
    violation: StdMutex<Option<String>>,
    progress: BackupProgress,
    tracking: AtomicBool,
    started: AtomicBool,
    meter: DeltaMeter,
}

impl BackupStorage {
    pub(crate) fn new(
        session: ObjectSession,
        sandbox: PathSandbox,
        progress: BackupProgress,
    ) -> Self {
        Self {
            session,
            sandbox,
            violation: StdMutex::new(None),
            progress,
            tracking: AtomicBool::new(false),
            started: AtomicBool::new(false),
            meter: DeltaMeter::default(),
        }
    }

    pub(crate) fn sandbox(&self) -> &PathSandbox {
        &self.sandbox
    }

    /// Ignore local preparation; the first storage op after the device request
    /// means its passcode gate has cleared and the DeviceLink loop has begun.
    fn touch(&self) {
        if self.tracking.load(Ordering::Relaxed) && !self.started.swap(true, Ordering::Relaxed) {
            // Nothing has moved yet, so the frame itself is the whole signal.
            self.progress.emit(BACKUP_PHASE_TRANSFER, -1.0, 0);
        }
    }

    pub(crate) fn begin_transfer(&self) {
        self.tracking.store(true, Ordering::Relaxed);
    }

    fn resolve_key(&self, path: &Path) -> Result<String, idevice::IdeviceError> {
        self.sandbox.resolve_key(path).inspect_err(|error| {
            let mut violation = crate::lock(&self.violation);
            if violation.is_none() {
                *violation = Some(error.to_string());
            }
        })
    }

    fn resolve_key_pair(
        &self,
        from: &Path,
        to: &Path,
    ) -> Result<(String, String), idevice::IdeviceError> {
        Ok((self.resolve_key(from)?, self.resolve_key(to)?))
    }

    pub(crate) fn violation(&self) -> Option<String> {
        crate::lock(&self.violation).clone()
    }
}

// Every op below (except create_dir_all, which the crate calls before the
// backup request goes out) only happens inside the DL loop → touch().
impl BackupDelegate for BackupStorage {
    fn get_free_disk_space(&self, _path: &Path) -> u64 {
        self.touch();
        self.session.free_disk_space()
    }
    fn open_file_read<'a>(
        &'a self,
        path: &'a Path,
    ) -> DelegateFuture<'a, Result<Box<dyn Read + Send>, idevice::IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let key = self.resolve_key(path)?;
            self.session.open_file_read(&key)
        })
    }
    fn create_file_write<'a>(
        &'a self,
        path: &'a Path,
    ) -> DelegateFuture<'a, Result<Box<dyn Write + Send>, idevice::IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let key = self.resolve_key(path)?;
            self.session.create_file_write(&key)
        })
    }
    fn create_dir_all<'a>(
        &'a self,
        path: &'a Path,
    ) -> DelegateFuture<'a, Result<(), idevice::IdeviceError>> {
        Box::pin(async move {
            let key = self.resolve_key(path)?;
            self.session.create_dir_all(&key)
        })
    }
    fn remove<'a>(
        &'a self,
        path: &'a Path,
    ) -> DelegateFuture<'a, Result<(), idevice::IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let key = self.resolve_key(path)?;
            self.session.remove(&key)
        })
    }
    fn rename<'a>(
        &'a self,
        from: &'a Path,
        to: &'a Path,
    ) -> DelegateFuture<'a, Result<(), idevice::IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let (from, to) = self.resolve_key_pair(from, to)?;
            self.session.rename(&from, &to)
        })
    }
    fn copy<'a>(
        &'a self,
        src: &'a Path,
        dst: &'a Path,
    ) -> DelegateFuture<'a, Result<(), idevice::IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let (src, dst) = self.resolve_key_pair(src, dst)?;
            self.session.copy(&src, &dst)
        })
    }
    fn exists<'a>(&'a self, path: &'a Path) -> DelegateFuture<'a, bool> {
        self.touch();
        Box::pin(async move {
            match self.resolve_key(path) {
                Ok(key) => self.session.exists(&key),
                Err(_) => false,
            }
        })
    }
    fn is_dir<'a>(&'a self, path: &'a Path) -> DelegateFuture<'a, bool> {
        self.touch();
        Box::pin(async move {
            match self.resolve_key(path) {
                Ok(key) => self.session.is_dir(&key),
                Err(_) => false,
            }
        })
    }
    fn list_dir<'a>(
        &'a self,
        path: &'a Path,
    ) -> DelegateFuture<'a, Result<Vec<DirEntryInfo>, idevice::IdeviceError>> {
        self.touch();
        Box::pin(async move {
            let key = self.resolve_key(path)?;
            self.session.list_dir(&key)
        })
    }
    // Both payload directions, at least once per 256 KiB and mid-file, so this
    // is the whole byte account as well as the percentage.
    fn on_progress(&self, progress: Mb2Progress) {
        self.started.store(true, Ordering::Relaxed); // real frame follows anyway
        self.progress.emit(
            BACKUP_PHASE_TRANSFER,
            progress.overall_progress,
            self.meter.advance(progress.session_bytes_done),
        );
    }
}

#[cfg(test)]
mod tests {
    use super::DeltaMeter;

    // The host sums frames, so every byte the crate counts must be handed over
    // exactly once across the whole session.
    #[test]
    fn frames_carry_each_byte_once() {
        let meter = DeltaMeter::default();
        let session = [0, 4096, 4096 + (1 << 20), 3 << 20];
        let total: u64 = session.iter().map(|done| meter.advance(*done)).sum();
        assert_eq!(total, 3 << 20);
    }

    // A batch boundary re-reports the same session total, and a stale frame can
    // trail a newer one; neither may inflate the host's sum.
    #[test]
    fn repeated_and_stale_frames_contribute_nothing() {
        let meter = DeltaMeter::default();
        assert_eq!(meter.advance(4096), 4096);
        assert_eq!(meter.advance(4096), 0);
        assert_eq!(meter.advance(1024), 0);
        assert_eq!(meter.advance(8192), 4096);
    }
}
