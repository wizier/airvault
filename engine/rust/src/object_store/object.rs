//! Streaming object I/O: the temp-file writer that hashes and renames into
//! the pool, and the reader that verifies content hashes during restore.

use std::fs::{self, File};
use std::io::{Read, Write};
use std::path::PathBuf;
use std::sync::Arc;

use sha2::{Digest, Sha256};

use super::manifest::{EntryKind, ManifestEntry};
use super::session::Inner;
use super::{object_pool_path, reject_symlinks, unix_now, ObjectFailure, OBJECT_PREFIX_LEN};

pub(super) struct ObjectWriter {
    pub(super) inner: Arc<Inner>,
    pub(super) key: String,
    pub(super) temporary: PathBuf,
    /// Open until `finalize`, which only runs from Drop.
    pub(super) file: Option<File>,
    pub(super) size: u64,
    pub(super) hasher: Sha256,
    pub(super) failed: bool,
}

impl Write for ObjectWriter {
    fn write(&mut self, buffer: &[u8]) -> std::io::Result<usize> {
        let file = self.file.as_mut().expect("object file is open until drop");
        match file.write(buffer) {
            Ok(0) if !buffer.is_empty() => {
                let error = std::io::Error::new(
                    std::io::ErrorKind::WriteZero,
                    "object writer made no progress",
                );
                self.fail(ObjectFailure::integrity(format!(
                    "write object {:?}: {error}",
                    self.key
                )));
                Err(error)
            }
            Ok(written) => {
                let Some(size) = self
                    .size
                    .checked_add(written as u64)
                    .filter(|size| *size <= i64::MAX as u64)
                else {
                    let error = std::io::Error::other("object exceeds the supported size");
                    self.fail(ObjectFailure::integrity(format!(
                        "write object {:?}: {error}",
                        self.key
                    )));
                    return Err(error);
                };
                self.size = size;
                self.hasher.update(&buffer[..written]);
                Ok(written)
            }
            Err(error) => {
                self.fail(ObjectFailure::from_io(
                    format!("write object {:?}", self.key),
                    &error,
                ));
                Err(error)
            }
        }
    }

    // Writes go straight to the file, which has no userspace buffer to flush.
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

impl Drop for ObjectWriter {
    fn drop(&mut self) {
        let result = self.finalize();
        let mut state = self.inner.state();
        state.open_writers = state.open_writers.saturating_sub(1);
        let outcome = result.and_then(|(entry, newly_created)| {
            if newly_created {
                state.written.insert(entry.object_ref.clone());
            }
            state
                .tree
                .insert_file(&self.key, entry.object_ref, entry.size, entry.modified_unix)
                .map_err(ObjectFailure::integrity)
        });
        if let Err(error) = outcome {
            state.error.get_or_insert(error);
        }
    }
}

impl ObjectWriter {
    fn fail(&mut self, failure: ObjectFailure) {
        self.failed = true;
        self.inner.record_failure(failure);
    }

    fn finalize(&mut self) -> Result<(ManifestEntry, bool), ObjectFailure> {
        // Close before renaming or removing: network filesystems (SMB) may
        // refuse either on a file that is still open.
        drop(self.file.take());
        if self.failed {
            let _ = fs::remove_file(&self.temporary);
            return Err(ObjectFailure::integrity(format!(
                "writing object for {:?} failed",
                self.key
            )));
        }
        let object_ref = format!("{:x}", std::mem::take(&mut self.hasher).finalize());
        let target = object_pool_path(&self.inner.root, &self.inner.source, &object_ref);
        let shard = object_ref[..OBJECT_PREFIX_LEN].to_string();
        if !self.inner.state().ready_shards.contains(&shard) {
            let parent = target
                .parent()
                .ok_or_else(|| ObjectFailure::integrity("object target has no parent"))?;
            fs::create_dir_all(parent)
                .map_err(|error| ObjectFailure::from_io("create final object directory", &error))?;
            reject_symlinks(&self.inner.root, parent).map_err(ObjectFailure::integrity)?;
            self.inner.state().ready_shards.insert(shard);
        }
        // One writer per source (store lease + single session), so the
        // lstat→rename window cannot race and the lstat doubles as the symlink
        // check.
        let newly_created = match fs::symlink_metadata(&target) {
            // Keep a stored copy of the right length: renaming this unsynced file
            // over it could lose bytes older snapshots rely on if power fails.
            Ok(metadata) if metadata.is_file() && metadata.len() == self.size => {
                fs::remove_file(&self.temporary).map_err(|error| {
                    ObjectFailure::from_io(
                        format!("discard deduplicated object {:?}", self.key),
                        &error,
                    )
                })?;
                false
            }
            // A wrong length is provable damage; these bytes hash to the name, so
            // republishing heals it.
            Ok(metadata) if metadata.is_file() => {
                tracing::warn!(
                    object = %object_ref,
                    expected = self.size,
                    found = metadata.len(),
                    "replacing a damaged pool object"
                );
                fs::rename(&self.temporary, &target).map_err(|error| {
                    ObjectFailure::from_io(
                        format!("republish deduplicated object {:?}", self.key),
                        &error,
                    )
                })?;
                false
            }
            Ok(_) => {
                return Err(ObjectFailure::integrity(format!(
                    "object pool path for {:?} is not a regular file",
                    self.key
                )))
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                fs::rename(&self.temporary, &target).map_err(|error| {
                    ObjectFailure::from_io(format!("publish object {:?}", self.key), &error)
                })?;
                true
            }
            Err(error) => {
                return Err(ObjectFailure::integrity(format!(
                    "inspect deduplicated object {:?}: {error}",
                    self.key
                )))
            }
        };
        Ok((
            ManifestEntry {
                kind: EntryKind::File,
                object_ref,
                size: self.size as i64,
                modified_unix: unix_now(),
            },
            newly_created,
        ))
    }
}

pub(super) struct HashVerifyReader {
    file: File,
    inner: Arc<Inner>,
    hasher: Sha256,
    remaining: u64,
    expected_hex: String,
    context: String,
    verified: bool,
    // First failure, replayed verbatim on any further read.
    failed: Option<(std::io::ErrorKind, String)>,
}

impl HashVerifyReader {
    pub(super) fn new(
        file: File,
        inner: Arc<Inner>,
        size: u64,
        expected_hex: String,
        context: String,
    ) -> Self {
        Self {
            file,
            inner,
            hasher: Sha256::new(),
            remaining: size,
            expected_hex,
            context,
            verified: false,
            failed: None,
        }
    }

    fn fail(&mut self, failure: ObjectFailure, kind: std::io::ErrorKind) -> std::io::Error {
        self.failed = Some((kind, failure.detail.clone()));
        self.inner.record_failure(failure);
        std::io::Error::new(kind, self.failed.as_ref().unwrap().1.clone())
    }

    fn fail_integrity(&mut self, kind: std::io::ErrorKind, message: &str) -> std::io::Error {
        let detail = format!("object {} {message}", self.context);
        self.fail(ObjectFailure::integrity(detail), kind)
    }

    fn verify(&mut self) -> std::io::Result<()> {
        let got = format!("{:x}", std::mem::take(&mut self.hasher).finalize());
        if got != self.expected_hex {
            return Err(
                self.fail_integrity(std::io::ErrorKind::InvalidData, "content hash mismatch")
            );
        }
        self.verified = true;
        Ok(())
    }
}

impl Read for HashVerifyReader {
    fn read(&mut self, buffer: &mut [u8]) -> std::io::Result<usize> {
        if let Some((kind, detail)) = &self.failed {
            return Err(std::io::Error::new(*kind, detail.clone()));
        }
        if self.verified {
            return Ok(0);
        }
        if self.remaining == 0 {
            self.verify()?;
            return Ok(0);
        }
        if buffer.is_empty() {
            return Ok(0);
        }
        let limit = buffer
            .len()
            .min(usize::try_from(self.remaining).unwrap_or(usize::MAX));
        let read = match self.file.read(&mut buffer[..limit]) {
            Ok(read) => read,
            Err(error) => {
                // A read error is an io condition, not proof of corruption.
                let detail = format!("object {} could not be read: {error}", self.context);
                return Err(self.fail(
                    ObjectFailure::from_io_kind(error.kind(), detail),
                    error.kind(),
                ));
            }
        };
        if read == 0 {
            return Err(self.fail_integrity(
                std::io::ErrorKind::UnexpectedEof,
                "ended before its manifest size",
            ));
        }
        self.hasher.update(&buffer[..read]);
        self.remaining -= read as u64;
        if self.remaining == 0 {
            self.verify()?;
        }
        Ok(read)
    }
}
