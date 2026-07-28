//! The single mobilebackup2 BackupDelegate: sandboxes device paths into
//! logical store keys, reports transfer progress, and records the first
//! sandbox violation, delegating storage to the session's object store.

use std::future::Future;
use std::path::Path;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex as StdMutex};

use idevice::services::mobilebackup2::{BackupDelegate, DirEntryInfo};
use std::io::{Read, Write};

use crate::object_store::ObjectSession;
use crate::path_sandbox::PathSandbox;

/// Progress callback into Go: (opaque operation id, phase, percent, done, total).
/// Phase is BACKUP_PHASE_*; percent < 0, done == 0, total == 0 each mean "not
/// reported this call". `done` is cumulative; `total` is the current batch size.
pub(crate) type BackupCb = extern "C" fn(usize, i32, f64, u64, u64);
pub(crate) const BACKUP_PHASE_TRANSFER: i32 = 0;
pub(crate) const BACKUP_PHASE_FINALIZING: i32 = 1;

#[derive(Clone, Copy)]
pub(crate) struct BackupProgress {
    pub(crate) callback: BackupCb,
    pub(crate) id: usize,
}

impl BackupProgress {
    pub(crate) fn emit(self, phase: i32, percent: f64, done: u64, total: u64) {
        (self.callback)(self.id, phase, percent, done, total);
    }
}

/// Monotonic run-total fed by both byte sources: cumulative per-batch upload
/// snapshots (backup) and CountingReader increments (restore). Atomics only
/// for Send + 'static readers — the DL loop itself is sequential.
#[derive(Default)]
pub(crate) struct TransferBytes {
    total: AtomicU64,
    batch: AtomicU64,
}

impl TransferBytes {
    pub(crate) fn begin_batch(&self) {
        self.batch.store(0, Ordering::Relaxed);
    }

    /// Upload path: fold the crate's cumulative per-batch value into the total.
    pub(crate) fn set_batch(&self, current: u64) -> u64 {
        let last = self.batch.swap(current, Ordering::Relaxed);
        self.add(current.saturating_sub(last))
    }

    fn add(&self, delta: u64) -> u64 {
        self.total.fetch_add(delta, Ordering::Relaxed) + delta
    }
}

/// Meters restore reads: the crate streams open_file_read to the device in
/// 32 KiB chunks. Every chunk reports; the host coalesces (latest-wins channel
/// plus a 200 ms emit window), so throttling here would only lose precision.
struct CountingReader {
    inner: Box<dyn Read + Send>,
    bytes: Arc<TransferBytes>,
    progress: BackupProgress,
}

impl Read for CountingReader {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let n = self.inner.read(buf)?;
        // Percent -1 (not reported) keeps the device-reported value intact.
        self.progress
            .emit(BACKUP_PHASE_TRANSFER, -1.0, self.bytes.add(n as u64), 0);
        Ok(n)
    }
}

type DelegateFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// Progress reporting and a per-source sandbox around one object session.
pub(crate) struct BackupStorage {
    session: ObjectSession,
    sandbox: PathSandbox,
    violation: StdMutex<Option<String>>,
    progress: BackupProgress,
    bytes: Arc<TransferBytes>,
    count_reads: bool,
    tracking: AtomicBool,
    started: AtomicBool,
}

impl BackupStorage {
    /// `count_reads` (restore): meter open_file_read streams into progress.
    /// Never set for backup — the device downloads Status.plist/Manifest.db
    /// there, and those reads must not inflate the stored transferred total.
    pub(crate) fn new(
        session: ObjectSession,
        sandbox: PathSandbox,
        progress: BackupProgress,
        count_reads: bool,
    ) -> Self {
        Self {
            session,
            sandbox,
            violation: StdMutex::new(None),
            progress,
            bytes: Arc::new(TransferBytes::default()),
            count_reads,
            tracking: AtomicBool::new(false),
            started: AtomicBool::new(false),
        }
    }

    pub(crate) fn sandbox(&self) -> &PathSandbox {
        &self.sandbox
    }

    fn emit(&self, percent: f64, done: u64, total: u64) {
        self.progress
            .emit(BACKUP_PHASE_TRANSFER, percent, done, total);
    }

    /// Ignore local preparation; the first storage op after the device request
    /// means its passcode gate has cleared and the DeviceLink loop has begun.
    fn touch(&self) {
        if self.tracking.load(Ordering::Relaxed) && !self.started.swap(true, Ordering::Relaxed) {
            self.emit(-1.0, 0, 0);
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
            let reader = self.session.open_file_read(&key)?;
            if !self.count_reads || !self.tracking.load(Ordering::Relaxed) {
                return Ok(reader);
            }
            Ok(Box::new(CountingReader {
                inner: reader,
                bytes: self.bytes.clone(),
                progress: self.progress,
            }) as Box<dyn Read + Send>)
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
    fn on_progress(&self, bytes_done: u64, bytes_total: u64, overall_progress: f64) {
        self.started.store(true, Ordering::Relaxed); // real frame follows anyway
        let done = self.bytes.set_batch(bytes_done);
        self.emit(overall_progress, done, bytes_total);
    }
    fn on_file_received(&self, _path: &str, file_count: u32) {
        if file_count == 1 {
            self.bytes.begin_batch();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::{BackupProgress, CountingReader, TransferBytes};
    use std::io::Read;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::Arc;

    #[test]
    fn transfer_bytes_fold_batches_and_increments() {
        let bytes = TransferBytes::default();
        bytes.begin_batch();
        assert_eq!(bytes.set_batch(100), 100);
        assert_eq!(bytes.set_batch(250), 250); // same batch grows by its delta
        bytes.begin_batch();
        assert_eq!(bytes.set_batch(200), 450);
        assert_eq!(bytes.set_batch(0), 450); // progress-only frame between batches
        assert_eq!(bytes.add(50), 500);
    }

    extern "C" fn record_done(id: usize, _phase: i32, _percent: f64, done: u64, _total: u64) {
        unsafe { &*(id as *const AtomicU64) }.store(done, Ordering::Relaxed);
    }

    // Reads accumulate across files and every chunk reports, so the last frame
    // of the transfer already carries the exact total.
    #[test]
    fn counting_readers_report_the_running_total_across_files() {
        let last_done = Box::new(AtomicU64::new(0));
        let bytes = Arc::new(TransferBytes::default());
        let progress = BackupProgress {
            callback: record_done,
            id: &*last_done as *const AtomicU64 as usize,
        };
        for size in [4096, 9] {
            let mut reader = CountingReader {
                inner: Box::new(std::io::repeat(7).take(size)),
                bytes: bytes.clone(),
                progress,
            };
            assert_eq!(
                std::io::copy(&mut reader, &mut std::io::sink()).unwrap(),
                size
            );
        }
        assert_eq!(last_done.load(Ordering::Relaxed), 4105);
    }
}
