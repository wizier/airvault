//! Maps mobilebackup2 host paths into logical store keys for one source.
//! Normal paths live under `<root>/<source>`; iOS' `/.b/...` staging namespace
//! is privately remapped under the source's protocol subtree. The output is a
//! manifest key — the store never performs filesystem I/O at the mapped path,
//! so no per-call symlink walk is needed here.

use std::ffi::OsStr;
use std::path::{Component, Path, PathBuf};

use crate::object_store::{validate_source, PROTOCOL_DIR};

const MAX_MB2_PATH_BYTES: usize = 4 * 1024;
// Counts host path components below the backup root, which is the manifest's
// MAX_LOGICAL_DEPTH plus a source or staging component — a different quantity,
// so the two limits stay separate even though both are 128.
const MAX_MB2_PATH_DEPTH: usize = 128;
const MAX_MB2_COMPONENT_BYTES: usize = 255;
const DEVICE_STAGING_DIR: &str = ".b";

fn rejected_path(reason: impl Into<String>) -> idevice::IdeviceError {
    idevice::IdeviceError::UnexpectedResponse(format!(
        "rejected unsafe mobilebackup2 host path: {}",
        reason.into()
    ))
}

fn normalize_path(path: &Path) -> Result<PathBuf, idevice::IdeviceError> {
    if path.as_os_str().is_empty() {
        return Err(rejected_path("empty path"));
    }
    let absolute = if path.is_absolute() {
        path.to_path_buf()
    } else {
        std::env::current_dir()
            .map_err(|e| rejected_path(format!("cannot resolve current directory: {e}")))?
            .join(path)
    };
    let mut normalized = PathBuf::new();
    for component in absolute.components() {
        match component {
            Component::Prefix(prefix) => normalized.push(prefix.as_os_str()),
            Component::RootDir => normalized.push(Component::RootDir.as_os_str()),
            Component::CurDir => {}
            Component::ParentDir => {
                if !normalized.pop() {
                    return Err(rejected_path("parent component escapes filesystem root"));
                }
            }
            Component::Normal(part) => normalized.push(part),
        }
    }
    Ok(normalized)
}

#[derive(Debug)]
pub(crate) struct PathSandbox {
    backup_root: PathBuf,
    allowed_source: String,
    allowed_root: PathBuf,
}

impl PathSandbox {
    pub(crate) fn new(root: &Path, source: &str) -> Result<Self, idevice::IdeviceError> {
        // One source-identifier contract for the whole store (Go enforces the same).
        validate_source(source).map_err(rejected_path)?;
        let backup_root = normalize_path(root)?;
        if backup_root.as_os_str().as_encoded_bytes().len() > MAX_MB2_PATH_BYTES {
            return Err(rejected_path("backup root exceeds path length limit"));
        }
        let allowed_root = backup_root.join(source);
        Ok(Self {
            backup_root,
            allowed_source: source.to_owned(),
            allowed_root,
        })
    }

    pub(crate) fn allowed_root(&self) -> &Path {
        &self.allowed_root
    }

    /// Resolves one mb2 host path into a logical manifest key ("" = source root).
    pub(crate) fn resolve_key(&self, path: &Path) -> Result<String, idevice::IdeviceError> {
        if path.as_os_str().as_encoded_bytes().len() > MAX_MB2_PATH_BYTES {
            return Err(rejected_path("path exceeds 4096-byte limit"));
        }
        let logical = normalize_path(path)?;
        let relative = logical
            .strip_prefix(&self.backup_root)
            .map_err(|_| rejected_path("path is outside the configured backup root"))?;
        let mut components = relative.components();
        let Some(Component::Normal(first)) = components.next() else {
            return Err(rejected_path("backup root is not an addressable path"));
        };

        let rest: Vec<&str> = components
            .map(|component| match component {
                Component::Normal(part) => key_component(part),
                _ => Err(rejected_path("path contains a non-normal component")),
            })
            .collect::<Result<_, _>>()?;
        if rest.len() + 1 > MAX_MB2_PATH_DEPTH {
            return Err(rejected_path("path exceeds 128-component depth limit"));
        }

        let key = if first == OsStr::new(&self.allowed_source) {
            if rest.first() == Some(&PROTOCOL_DIR) {
                return Err(rejected_path(
                    "path addresses the reserved protocol subtree",
                ));
            }
            rest.join("/")
        } else if first == OsStr::new(DEVICE_STAGING_DIR) {
            let mut key = format!("{PROTOCOL_DIR}/{DEVICE_STAGING_DIR}");
            for part in &rest {
                key.push('/');
                key.push_str(part);
            }
            key
        } else {
            return Err(rejected_path(format!(
                "first component {first:?} is neither the allowed source nor the staging namespace"
            )));
        };
        if key.len() > MAX_MB2_PATH_BYTES {
            return Err(rejected_path("mapped key exceeds 4096-byte limit"));
        }
        Ok(key)
    }
}

/// One validated key component: UTF-8, bounded, and free of the characters the
/// manifest contract forbids in logical paths.
fn key_component(part: &OsStr) -> Result<&str, idevice::IdeviceError> {
    if part.as_encoded_bytes().len() > MAX_MB2_COMPONENT_BYTES {
        return Err(rejected_path("path component exceeds 255-byte limit"));
    }
    let part = part
        .to_str()
        .ok_or_else(|| rejected_path("path is not valid UTF-8"))?;
    if part.is_empty()
        || part == "."
        || part == ".."
        || part.contains(['/', '\\', '\0', '\u{2028}', '\u{2029}'])
    {
        return Err(rejected_path("path contains an invalid component"));
    }
    Ok(part)
}
