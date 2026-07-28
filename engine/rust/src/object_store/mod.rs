//! AirVault's portable whole-file snapshot format: a per-source pool of
//! SHA-256-addressed immutable objects plus one sealed manifest per snapshot.

mod manifest;
mod object;
mod session;
mod tree;

pub(crate) use session::ObjectSession;

use std::path::{Component, Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tokio_util::sync::CancellationToken;

const VERSION: u32 = 1;
pub(crate) const PROTOCOL_DIR: &str = ".airvault-protocol";
const OBJECT_PREFIX_LEN: usize = 2;
const MAX_MANIFEST_BYTES: u64 = 512 << 20;
const MAX_ENTRIES: usize = 2_000_000;
const MAX_LOGICAL_DEPTH: usize = 128;

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) enum ObjectFailureKind {
    StorageFull,
    Integrity,
    Internal,
}

#[derive(Clone, Debug)]
pub(crate) struct ObjectFailure {
    pub(crate) kind: ObjectFailureKind,
    pub(crate) detail: String,
}

impl ObjectFailure {
    pub(crate) fn integrity(detail: impl Into<String>) -> Self {
        Self {
            kind: ObjectFailureKind::Integrity,
            detail: detail.into(),
        }
    }

    pub(crate) fn from_io(context: impl Into<String>, error: &std::io::Error) -> Self {
        Self::from_io_kind(error.kind(), format!("{}: {error}", context.into()))
    }

    // A serde error either wraps io (classified like any io error) or judges
    // the bytes themselves (integrity).
    pub(crate) fn from_serde(context: impl Into<String>, error: &serde_json::Error) -> Self {
        let detail = format!("{}: {error}", context.into());
        match error.io_error_kind() {
            Some(kind) => Self::from_io_kind(kind, detail),
            None => Self::integrity(detail),
        }
    }

    pub(crate) fn from_io_kind(kind: std::io::ErrorKind, detail: impl Into<String>) -> Self {
        let kind = match kind {
            std::io::ErrorKind::StorageFull | std::io::ErrorKind::QuotaExceeded => {
                ObjectFailureKind::StorageFull
            }
            _ => ObjectFailureKind::Internal,
        };
        Self {
            kind,
            detail: detail.into(),
        }
    }
}

// Bare-string failures are always validation/content findings, never io.
impl From<String> for ObjectFailure {
    fn from(detail: String) -> Self {
        Self::integrity(detail)
    }
}

fn snapshot_manifest_path(root: &Path, source: &str, snapshot_id: &str) -> Result<PathBuf, String> {
    validate_source(source)?;
    validate_snapshot_id(snapshot_id)?;
    let relative = format!("{source}/snapshots/{snapshot_id}.json");
    let target = root.join(relative_components(&relative)?);
    reject_symlinks(root, &target)?;
    Ok(target)
}

/// Pool path of an object without the per-component symlink walk. Publish
/// validates the shard directory once per session and lstats the target itself.
fn object_pool_path(root: &Path, source: &str, object_ref: &str) -> Result<PathBuf, String> {
    validate_source(source)?;
    validate_object_ref(object_ref)?;
    let relative = format!(
        "{source}/objects/{}/{object_ref}",
        &object_ref[..OBJECT_PREFIX_LEN]
    );
    Ok(root.join(relative_components(&relative)?))
}

fn resolve_object_ref(root: &Path, source: &str, object_ref: &str) -> Result<PathBuf, String> {
    let target = object_pool_path(root, source, object_ref)?;
    reject_symlinks(root, &target)?;
    Ok(target)
}

fn validate_object_ref(object_ref: &str) -> Result<(), String> {
    if !is_sha256_hex(object_ref) {
        return Err(format!("invalid object reference {object_ref:?}"));
    }
    Ok(())
}

fn relative_components(relative: &str) -> Result<PathBuf, String> {
    // U+2028/U+2029 stay out of every stored path: line separators some JSON
    // tooling mishandles.
    if relative.is_empty()
        || relative.len() > 4096
        || relative.contains(['\\', '\0', '\u{2028}', '\u{2029}'])
    {
        return Err(format!("invalid relative storage path {relative:?}"));
    }
    let mut output = PathBuf::new();
    for component in Path::new(relative).components() {
        match component {
            Component::Normal(part) if part != std::ffi::OsStr::new("") => output.push(part),
            _ => return Err(format!("invalid relative storage path {relative:?}")),
        }
    }
    Ok(output)
}

fn reject_symlinks(root: &Path, target: &Path) -> Result<(), String> {
    let relative = target
        .strip_prefix(root)
        .map_err(|_| "object path escaped backup root".to_string())?;
    let mut current = root.to_path_buf();
    for component in relative.components() {
        let Component::Normal(part) = component else {
            return Err("object path contains an invalid component".into());
        };
        current.push(part);
        match std::fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                return Err(format!("object path crosses symlink {}", current.display()))
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => break,
            Err(error) => return Err(format!("inspect object path: {error}")),
        }
    }
    Ok(())
}

pub(crate) fn validate_source(source: &str) -> Result<(), String> {
    if source.is_empty()
        || source.len() > 64
        || !source
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        || !source.bytes().any(|byte| byte.is_ascii_alphanumeric())
    {
        return Err("source must be 1..64 ASCII letters, digits, or hyphens".into());
    }
    Ok(())
}

fn validate_snapshot_id(value: &str) -> Result<(), String> {
    let parsed = uuid::Uuid::parse_str(value)
        .map_err(|_| format!("snapshot id {value:?} is not a canonical UUID"))?;
    if parsed.hyphenated().to_string() != value {
        return Err(format!("snapshot id {value:?} is not a canonical UUID"));
    }
    Ok(())
}

fn hex_lower(bytes: &[u8]) -> String {
    let mut out = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        out.push(char::from_digit((byte >> 4) as u32, 16).unwrap());
        out.push(char::from_digit((byte & 0x0f) as u32, 16).unwrap());
    }
    out
}

/// Lowercase hex only — the Go checksum contract stores object hashes lowercase.
fn is_sha256_hex(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn unix_now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64
}

fn system_time(unix: i64) -> Option<SystemTime> {
    (unix > 0).then(|| UNIX_EPOCH + Duration::from_secs(unix as u64))
}

fn not_cancelled(cancel: &CancellationToken) -> Result<(), String> {
    if cancel.is_cancelled() {
        Err("backup cancelled".into())
    } else {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn io_errors_have_stable_failure_kinds() {
        let storage_full = std::io::Error::new(std::io::ErrorKind::StorageFull, "disk full");
        assert_eq!(
            ObjectFailure::from_io("write object", &storage_full).kind,
            ObjectFailureKind::StorageFull
        );
        let quota = std::io::Error::new(std::io::ErrorKind::QuotaExceeded, "quota exceeded");
        assert_eq!(
            ObjectFailure::from_io("write object", &quota).kind,
            ObjectFailureKind::StorageFull
        );
        let denied = std::io::Error::new(std::io::ErrorKind::PermissionDenied, "denied");
        assert_eq!(
            ObjectFailure::from_io("write object", &denied).kind,
            ObjectFailureKind::Internal
        );
    }
}
