//! The sealed per-snapshot manifest: the portable wire contract shared with
//! the Go validator (entries seal must stay byte-identical across languages).

use std::collections::BTreeMap;
use std::fs::{self, File};
use std::io::BufReader;
use std::path::Path;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use super::{
    hex_lower, is_sha256_hex, relative_components, snapshot_manifest_path, validate_object_ref,
    validate_snapshot_id, ObjectFailure, MAX_ENTRIES, MAX_LOGICAL_DEPTH, MAX_MANIFEST_BYTES,
    VERSION,
};

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub(super) struct ManifestEntry {
    pub(super) kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub(super) object_ref: String,
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub(super) size: i64,
    #[serde(default, skip_serializing_if = "is_zero_i64")]
    pub(super) modified_unix: i64,
}

#[derive(Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
pub(super) struct Manifest {
    pub(super) version: u32,
    pub(super) source_udid: String,
    pub(super) snapshot_id: String,
    pub(super) created_unix: i64,
    pub(super) size_bytes: i64,
    pub(super) entries_sha256: String,
    pub(super) entries: BTreeMap<String, ManifestEntry>,
}

fn is_zero_i64(value: &i64) -> bool {
    *value == 0
}

// IO failures keep their io classification (a missing file or NAS hiccup is
// not data corruption); only content mismatches surface as Integrity.
pub(super) fn load_manifest(
    root: &Path,
    source: &str,
    snapshot_id: &str,
) -> Result<Manifest, ObjectFailure> {
    let manifest_path = snapshot_manifest_path(root, source, snapshot_id)?;
    let metadata = fs::metadata(&manifest_path).map_err(|error| {
        ObjectFailure::from_io(
            format!("inspect object manifest {}", manifest_path.display()),
            &error,
        )
    })?;
    if !metadata.is_file() || metadata.len() == 0 || metadata.len() > MAX_MANIFEST_BYTES {
        return Err(ObjectFailure::integrity(format!(
            "object manifest {} has invalid size",
            manifest_path.display()
        )));
    }
    let file = File::open(&manifest_path).map_err(|error| {
        ObjectFailure::from_io(
            format!("open object manifest {}", manifest_path.display()),
            &error,
        )
    })?;
    let manifest: Manifest = serde_json::from_reader(BufReader::new(file))
        .map_err(|error| ObjectFailure::from_serde("decode object manifest", &error))?;
    validate_manifest(source, &manifest)?;
    if manifest.snapshot_id != snapshot_id {
        return Err(ObjectFailure::integrity(format!(
            "object manifest snapshot {:?} does not match {:?}",
            manifest.snapshot_id, snapshot_id
        )));
    }
    Ok(manifest)
}

pub(super) fn validate_manifest(source: &str, manifest: &Manifest) -> Result<(), String> {
    validate_manifest_header(source, manifest)?;
    let facts = inspect_manifest_entries(&manifest.entries, || false)?;
    if facts.size_bytes != manifest.size_bytes {
        return Err("object manifest snapshot size mismatch".into());
    }
    if facts.entries_sha256 != manifest.entries_sha256 {
        return Err("object manifest checksum mismatch".into());
    }
    Ok(())
}

// Debug: the tests' unwrap_err over Result<EntryFacts, _> needs it.
#[derive(Debug)]
pub(super) struct EntryFacts {
    pub(super) size_bytes: i64,
    pub(super) entries_sha256: String,
}

// Seals one entry into the running digest: a length-prefixed, little-endian
// encoding taken in BTreeMap (byte-lexicographic key) order, which Go's
// manifestEntriesSeal reproduces byte-for-byte
fn seal_entry(digest: &mut Sha256, key: &str, entry: &ManifestEntry) {
    seal_field(digest, key.as_bytes());
    seal_field(digest, entry.kind.as_bytes());
    seal_field(digest, entry.object_ref.as_bytes());
    digest.update(entry.size.to_le_bytes());
    digest.update(entry.modified_unix.to_le_bytes());
}

fn seal_field(digest: &mut Sha256, bytes: &[u8]) {
    digest.update((bytes.len() as u64).to_le_bytes());
    digest.update(bytes);
}

pub(super) fn validate_manifest_header(source: &str, manifest: &Manifest) -> Result<(), String> {
    if manifest.version != VERSION {
        return Err("unsupported object manifest format".into());
    }
    if manifest.source_udid != source {
        return Err(format!(
            "object manifest source UDID {:?} does not match {:?}",
            manifest.source_udid, source
        ));
    }
    validate_snapshot_id(&manifest.snapshot_id)?;
    if manifest.created_unix <= 0
        || manifest.size_bytes < 0
        || !is_sha256_hex(&manifest.entries_sha256)
    {
        return Err("object manifest has invalid aggregate metadata".into());
    }
    if manifest.entries.len() > MAX_ENTRIES {
        return Err(format!("object manifest exceeds {MAX_ENTRIES} entries"));
    }
    Ok(())
}

// Validates the entry graph, totals every file entry (duplicates included) and
// builds the seal in one BTreeMap pass.
pub(super) fn inspect_manifest_entries(
    entries: &BTreeMap<String, ManifestEntry>,
    cancelled: impl Fn() -> bool,
) -> Result<EntryFacts, String> {
    let mut snapshot_size = 0i64;
    let mut digest = Sha256::new();
    for (key, entry) in entries {
        if cancelled() {
            return Err("backup cancelled".into());
        }
        validate_logical_key(key)?;
        let depth = key.split('/').count();
        if depth > MAX_LOGICAL_DEPTH {
            return Err(format!(
                "object manifest path {key:?} exceeds {MAX_LOGICAL_DEPTH} components"
            ));
        }
        if let Some((parent, _)) = key.rsplit_once('/') {
            match entries.get(parent) {
                Some(parent_entry) if parent_entry.kind == "directory" => {}
                Some(_) => {
                    return Err(format!(
                        "object manifest parent {parent:?} of {key:?} is not a directory"
                    ))
                }
                None => {
                    return Err(format!(
                        "object manifest path {key:?} is missing parent directory {parent:?}"
                    ))
                }
            }
        }
        match entry.kind.as_str() {
            "directory" => {
                if !entry.object_ref.is_empty() || entry.size != 0 {
                    return Err(format!(
                        "object manifest directory {key:?} has file content"
                    ));
                }
            }
            "file" => {
                if entry.size < 0 {
                    return Err(format!("object manifest file {key:?} has negative size"));
                }
                validate_object_ref(&entry.object_ref)?;
                snapshot_size = snapshot_size
                    .checked_add(entry.size)
                    .ok_or_else(|| "object manifest snapshot size overflow".to_string())?;
            }
            _ => {
                return Err(format!(
                    "object manifest path {key:?} has invalid kind {:?}",
                    entry.kind
                ))
            }
        }
        seal_entry(&mut digest, key, entry);
    }
    Ok(EntryFacts {
        size_bytes: snapshot_size,
        entries_sha256: hex_lower(&digest.finalize()),
    })
}

pub(super) fn validate_logical_key(key: &str) -> Result<(), String> {
    let normalized = relative_components(key)?
        .to_string_lossy()
        .replace(std::path::MAIN_SEPARATOR, "/");
    if normalized != key {
        return Err(format!("non-canonical object logical path {key:?}"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    // Seals a finished entry map: fixtures and the wire-format golden.
    fn entries_checksum(entries: &BTreeMap<String, ManifestEntry>) -> String {
        let mut digest = Sha256::new();
        for (key, entry) in entries {
            seal_entry(&mut digest, key, entry);
        }
        hex_lower(&digest.finalize())
    }

    fn single_file_manifest() -> Manifest {
        let mut entries = BTreeMap::new();
        entries.insert(
            "Manifest.db".to_string(),
            ManifestEntry {
                kind: "file".into(),
                object_ref: "11".repeat(32),
                size: 100,
                modified_unix: 0,
            },
        );
        let mut manifest = Manifest {
            version: VERSION,
            source_udid: "testudid01".into(),
            snapshot_id: "aaaaaaaa-0000-4000-8000-000000000001".into(),
            created_unix: 1,
            size_bytes: 100,
            entries_sha256: String::new(),
            entries,
        };
        manifest.entries_sha256 = entries_checksum(&manifest.entries);
        manifest
    }

    // A flipped object reference keeps the same size (so the size check passes),
    // yet would silently restore a different object — the seal must catch it.
    #[test]
    fn manifest_checksum_detects_object_ref_tampering() {
        let mut manifest = single_file_manifest();
        validate_manifest("testudid01", &manifest).expect("untampered manifest validates");

        manifest.entries.get_mut("Manifest.db").unwrap().object_ref = "22".repeat(32);
        let err = validate_manifest("testudid01", &manifest).unwrap_err();
        assert!(err.contains("checksum mismatch"), "got: {err}");
    }

    #[test]
    fn manifest_rejects_invalid_content_address() {
        let entries = BTreeMap::from([(
            "file".to_string(),
            ManifestEntry {
                kind: "file".into(),
                object_ref: "not-a-sha256".into(),
                size: 1,
                modified_unix: 0,
            },
        )]);
        let error = inspect_manifest_entries(&entries, || false).unwrap_err();
        assert!(error.contains("invalid object reference"));
    }

    #[test]
    fn manifest_requires_explicit_directory_parents() {
        let entries = BTreeMap::from([(
            "missing/file".to_string(),
            ManifestEntry {
                kind: "file".into(),
                object_ref: "11".repeat(32),
                size: 1,
                modified_unix: 0,
            },
        )]);

        let error = inspect_manifest_entries(&entries, || false).unwrap_err();
        assert!(error.contains("missing parent directory"), "got: {error}");

        let entries = BTreeMap::from([
            (
                "parent".to_string(),
                ManifestEntry {
                    kind: "file".into(),
                    object_ref: "11".repeat(32),
                    size: 1,
                    modified_unix: 0,
                },
            ),
            (
                "parent/child".to_string(),
                ManifestEntry {
                    kind: "file".into(),
                    object_ref: "22".repeat(32),
                    size: 1,
                    modified_unix: 0,
                },
            ),
        ]);
        let error = inspect_manifest_entries(&entries, || false).unwrap_err();
        assert!(error.contains("is not a directory"), "got: {error}");
    }

    #[test]
    fn manifest_rejects_excessive_logical_depth() {
        let key = std::iter::repeat_n("a", MAX_LOGICAL_DEPTH + 1)
            .collect::<Vec<_>>()
            .join("/");
        let entries = BTreeMap::from([(
            key,
            ManifestEntry {
                kind: "directory".into(),
                object_ref: String::new(),
                size: 0,
                modified_unix: 0,
            },
        )]);

        let error = inspect_manifest_entries(&entries, || false).unwrap_err();
        assert!(error.contains("exceeds 128 components"), "got: {error}");
    }

    // Golden vectors shared with Go (TestEntriesChecksumMatchesRustVector): both
    // must hash identically or the seal is unportable. The multi-entry vector
    // pins byte-wise key order ("B" < "a" < UTF-8 "а").
    #[test]
    fn manifest_seal_matches_go_golden() {
        let mut entries = BTreeMap::new();
        entries.insert(
            "A<&".to_string(),
            ManifestEntry {
                kind: "file".into(),
                object_ref: "11".repeat(32),
                size: 7,
                modified_unix: 9,
            },
        );
        assert_eq!(
            entries_checksum(&entries),
            "98bcef975d7c1daa1f331dd9aad8555d15335fcd07c17062ce24380e999aa176"
        );

        let mut entries = BTreeMap::new();
        entries.insert(
            "a".to_string(),
            ManifestEntry {
                kind: "file".into(),
                object_ref: "11".repeat(32),
                size: 1,
                modified_unix: 2,
            },
        );
        entries.insert(
            "B".to_string(),
            ManifestEntry {
                kind: "directory".into(),
                object_ref: String::new(),
                size: 0,
                modified_unix: 0,
            },
        );
        entries.insert(
            "а".to_string(),
            ManifestEntry {
                kind: "file".into(),
                object_ref: "22".repeat(32),
                size: 3,
                modified_unix: 4,
            },
        );
        assert_eq!(
            entries_checksum(&entries),
            "42077a9ba0fdb9f9027e23b72e0bb7bfc192a161fc5b8a80127ec3b43cb5a621"
        );
    }
}
