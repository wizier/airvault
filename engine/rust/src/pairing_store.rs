//! AirVault's own pairing-record store (one plist per udid under the engine's
//! pairing root) — not the system usbmuxd/Finder store — answers "did AirVault
//! pair this". Records hold private keys: 0600 files, 0700 dirs, atomic durable writes.

use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{LazyLock, Mutex};

use idevice::pairing_file::PairingFile;

use crate::engine_error::{EngineFailure, ErrorKind};
use crate::provider::MAX_UDID_BYTES;

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

// A local store failure is the host's own fault, never a device verdict.
impl From<PairingStoreError> for EngineFailure {
    fn from(error: PairingStoreError) -> Self {
        Self::new(ErrorKind::Internal, error.to_string())
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
pub(crate) struct PairingStore {
    pub(crate) root: PathBuf,
}

impl PairingStore {
    pub(crate) fn new(root: impl Into<PathBuf>) -> Self {
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
    pub(crate) fn load_pairing(
        &self,
        udid: &str,
    ) -> Result<Option<PairingFile>, PairingStoreError> {
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
    pub(crate) fn save_pairing(
        &self,
        udid: &str,
        pairing: &PairingFile,
    ) -> Result<(), PairingStoreError> {
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
    pub(crate) fn delete_pairing(&self, udid: &str) -> Result<bool, PairingStoreError> {
        let path = self.pairing_path(udid)?;
        if !self.ensure_root(false)? {
            return Ok(false);
        }
        remove_private_file(&self.root, &path)
    }

    pub(crate) fn load_identity(
        &self,
        udid: &str,
    ) -> Result<Option<PairingIdentity>, PairingStoreError> {
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

    pub(crate) fn persist_identity_if_absent(
        &self,
        udid: &str,
        candidate: PairingIdentity,
    ) -> Result<PairingIdentity, PairingStoreError> {
        let _guard = crate::lock(&PAIRING_IDENTITY_LOCK);
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

    pub(crate) fn delete_identity(&self, udid: &str) -> Result<bool, PairingStoreError> {
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
    // Record paths always end in an ASCII `<udid>.plist` (see file_name).
    let file_name = path.file_name().unwrap_or_default().to_string_lossy();
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
