//! The single mobilebackup2 BackupDelegate: sandboxes device paths into
//! logical store keys, reports transfer progress, and records the first
//! sandbox violation, delegating storage to the session's object store.

use std::future::Future;
use std::path::Path;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex as StdMutex;

use idevice::services::mobilebackup2::{BackupDelegate, DirEntryInfo};
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

/// Payload reported per frame. Device blocks are far smaller, and the host
/// coalesces regardless, so batching keeps a fast link from spending thousands
/// of callbacks a second.
const METER_FRAME_BYTES: u64 = 1 << 20;

/// Meters one payload stream: object writes while backing up, object reads
/// while restoring. The crate's own byte reporting lands once per completed
/// file, too coarse to show a multi-gigabyte one moving.
struct Metered<T> {
    inner: T,
    progress: BackupProgress,
    pending: u64,
}

impl<T> Metered<T> {
    fn record(&mut self, count: usize) {
        self.pending += count as u64;
        if self.pending >= METER_FRAME_BYTES {
            self.progress
                .emit(BACKUP_PHASE_TRANSFER, -1.0, self.pending);
            self.pending = 0;
        }
    }
}

/// Most files never reach a full frame, so the tail carries their whole size.
impl<T> Drop for Metered<T> {
    fn drop(&mut self) {
        if self.pending > 0 {
            self.progress
                .emit(BACKUP_PHASE_TRANSFER, -1.0, self.pending);
        }
    }
}

impl Read for Metered<Box<dyn Read + Send>> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let n = self.inner.read(buf)?;
        self.record(n);
        Ok(n)
    }
}

impl Write for Metered<Box<dyn Write + Send>> {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        let n = self.inner.write(buf)?;
        self.record(n);
        Ok(n)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        self.inner.flush()
    }
}

type DelegateFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// Progress reporting and a per-source sandbox around one object session.
pub(crate) struct BackupStorage {
    session: ObjectSession,
    sandbox: PathSandbox,
    violation: StdMutex<Option<String>>,
    progress: BackupProgress,
    restore: bool,
    tracking: AtomicBool,
    started: AtomicBool,
}

impl BackupStorage {
    /// `restore` selects the metered direction: reads streamed to the device on
    /// a restore, writes coming off it on a backup. The other direction carries
    /// mb2 protocol files, which must not inflate the transferred total.
    pub(crate) fn new(
        session: ObjectSession,
        sandbox: PathSandbox,
        progress: BackupProgress,
        restore: bool,
    ) -> Self {
        Self {
            session,
            sandbox,
            violation: StdMutex::new(None),
            progress,
            restore,
            tracking: AtomicBool::new(false),
            started: AtomicBool::new(false),
        }
    }

    pub(crate) fn sandbox(&self) -> &PathSandbox {
        &self.sandbox
    }

    /// Percentage-only frame; bytes travel on the metered stream.
    fn emit(&self, percent: f64) {
        self.progress.emit(BACKUP_PHASE_TRANSFER, percent, 0);
    }

    /// Ignore local preparation; the first storage op after the device request
    /// means its passcode gate has cleared and the DeviceLink loop has begun.
    fn touch(&self) {
        if self.tracking.load(Ordering::Relaxed) && !self.started.swap(true, Ordering::Relaxed) {
            self.emit(-1.0);
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
            if !self.restore || !self.tracking.load(Ordering::Relaxed) {
                return Ok(reader);
            }
            Ok(Box::new(Metered {
                inner: reader,
                progress: self.progress,
                pending: 0,
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
            let writer = self.session.create_file_write(&key)?;
            if self.restore || !self.tracking.load(Ordering::Relaxed) {
                return Ok(writer);
            }
            Ok(Box::new(Metered {
                inner: writer,
                progress: self.progress,
                pending: 0,
            }) as Box<dyn Write + Send>)
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
    // The metered stream counted these bytes; only the percentage is news.
    fn on_progress(&self, _bytes_done: u64, _bytes_total: u64, overall_progress: f64) {
        self.started.store(true, Ordering::Relaxed); // real frame follows anyway
        self.emit(overall_progress);
    }
}

#[cfg(test)]
mod tests {
    use super::{BackupProgress, Metered, METER_FRAME_BYTES};
    use std::io::{Read, Write};
    use std::sync::atomic::{AtomicU64, Ordering};

    // Sums frames the way the host does, so the assertions cover the delta
    // contract and not just this side's arithmetic.
    #[derive(Default)]
    struct Recorder {
        bytes: AtomicU64,
        frames: AtomicU64,
    }

    extern "C" fn record(id: usize, _phase: i32, _percent: f64, bytes: u64) {
        let recorder = unsafe { &*(id as *const Recorder) };
        recorder.bytes.fetch_add(bytes, Ordering::Relaxed);
        recorder.frames.fetch_add(1, Ordering::Relaxed);
    }

    fn progress(recorder: &Recorder) -> BackupProgress {
        BackupProgress {
            callback: record,
            id: recorder as *const Recorder as usize,
        }
    }

    // Restore direction, and most of a backup: a file below one frame reports
    // its whole size when the stream closes.
    #[test]
    fn metered_reads_sum_across_files() {
        let recorder = Recorder::default();
        for size in [4096, 9] {
            let mut reader = Metered {
                inner: Box::new(std::io::repeat(7).take(size)) as Box<dyn Read + Send>,
                progress: progress(&recorder),
                pending: 0,
            };
            assert_eq!(
                std::io::copy(&mut reader, &mut std::io::sink()).unwrap(),
                size
            );
        }
        assert_eq!(recorder.bytes.load(Ordering::Relaxed), 4105);
    }

    // Backup direction: a file past one frame reports mid-write, and the tail
    // below the next frame still lands.
    #[test]
    fn metered_writes_report_within_a_single_file() {
        let recorder = Recorder::default();
        let chunk = [7u8; 64 << 10];
        let written = 20 * chunk.len() as u64; // 1.25 frames
        {
            let mut writer = Metered {
                inner: Box::new(std::io::sink()) as Box<dyn Write + Send>,
                progress: progress(&recorder),
                pending: 0,
            };
            for _ in 0..20 {
                writer.write_all(&chunk).unwrap();
            }
            assert_eq!(recorder.bytes.load(Ordering::Relaxed), METER_FRAME_BYTES);
            assert_eq!(recorder.frames.load(Ordering::Relaxed), 1);
        }

        assert_eq!(recorder.bytes.load(Ordering::Relaxed), written);
        assert_eq!(recorder.frames.load(Ordering::Relaxed), 2);
    }
}
