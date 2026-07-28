//! One backup/restore session over the store: the in-memory snapshot tree plus
//! CAS object I/O and the final sealed publish.

use std::collections::BTreeSet;
use std::fs::{self, File, OpenOptions};
use std::io::{BufWriter, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Instant;

use idevice::services::mobilebackup2::{BackupDelegate, DirEntryInfo, FsBackupDelegate};
use idevice::IdeviceError;
use sha2::{Digest, Sha256};
use tokio_util::sync::CancellationToken;

use super::manifest::{
    inspect_manifest_entries, load_manifest, validate_manifest_header, Manifest,
};
use super::object::{HashVerifyReader, ObjectWriter};
use super::tree::{Node, NodeKind, Tree};
use super::{
    not_cancelled, reject_symlinks, relative_components, resolve_object_ref, system_time, unix_now,
    validate_object_ref, validate_snapshot_id, validate_source, ObjectFailure, MAX_MANIFEST_BYTES,
    PROTOCOL_DIR, VERSION,
};

pub(super) struct State {
    pub(super) tree: Tree,
    pub(super) error: Option<ObjectFailure>,
    pub(super) open_writers: usize,
    // Content addresses newly added to the pool this run, so finish() can delete
    // those the final manifest no longer references without scanning the pool.
    pub(super) written: BTreeSet<String>,
    // Shard dirs created+symlink-checked this session: publish needs only
    // lstat+rename per object. Safe because a source has a single writer.
    pub(super) ready_shards: BTreeSet<String>,
}

impl State {
    fn new(tree: Tree) -> Self {
        Self {
            tree,
            error: None,
            open_writers: 0,
            written: BTreeSet::new(),
            ready_shards: BTreeSet::new(),
        }
    }
}

pub(super) struct Inner {
    pub(super) root: PathBuf,
    pub(super) source: String,
    pub(super) snapshot_id: String,
    pub(super) staging_dir: Option<PathBuf>,
    pub(super) state: Mutex<State>,
}

impl Inner {
    pub(super) fn state(&self) -> std::sync::MutexGuard<'_, State> {
        crate::lock(&self.state)
    }

    pub(super) fn record_failure(&self, failure: ObjectFailure) {
        let mut state = self.state();
        if state.error.is_none() {
            state.error = Some(failure);
        }
    }
}

struct CancelWriter<'a, W> {
    inner: W,
    cancel: &'a CancellationToken,
}

impl<W: Write> Write for CancelWriter<'_, W> {
    fn write(&mut self, buffer: &[u8]) -> std::io::Result<usize> {
        if self.cancel.is_cancelled() {
            return Err(std::io::Error::new(
                std::io::ErrorKind::Interrupted,
                "backup cancelled",
            ));
        }
        self.inner.write(buffer)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        self.inner.flush()
    }
}

/// One session over content-addressed whole-file objects and one complete
/// manifest per snapshot. An object's bytes always hash to its name, so writing
/// the same content again republishes it; entries are addressed by logical key.
#[derive(Clone)]
pub(crate) struct ObjectSession {
    inner: Arc<Inner>,
}

impl ObjectSession {
    pub(crate) fn backup(
        root: &Path,
        source: &str,
        snapshot_id: &str,
        base_snapshot_id: Option<&str>,
    ) -> Result<Self, ObjectFailure> {
        validate_source(source)?;
        validate_snapshot_id(snapshot_id)?;
        let staging_dir = root.join(relative_components(&format!(
            "{source}/staging/{snapshot_id}"
        ))?);
        reject_symlinks(root, &staging_dir)?;
        if staging_dir.exists() {
            return Err(ObjectFailure::integrity(format!(
                "object staging directory already exists: {}",
                staging_dir.display()
            )));
        }
        fs::create_dir_all(staging_dir.join("objects"))
            .map_err(|error| ObjectFailure::from_io("create object staging directory", &error))?;

        let tree = match base_snapshot_id.filter(|value| !value.is_empty()) {
            Some(base_snapshot_id) => {
                Tree::from_entries(&load_manifest(root, source, base_snapshot_id)?.entries)?
            }
            None => Tree::new(),
        };
        Ok(Self {
            inner: Arc::new(Inner {
                root: root.to_path_buf(),
                source: source.into(),
                snapshot_id: snapshot_id.into(),
                staging_dir: Some(staging_dir),
                state: Mutex::new(State::new(tree)),
            }),
        })
    }

    pub(crate) fn restore(
        root: &Path,
        source: &str,
        snapshot_id: &str,
    ) -> Result<Self, ObjectFailure> {
        let tree = Tree::from_entries(&load_manifest(root, source, snapshot_id)?.entries)?;
        Ok(Self {
            inner: Arc::new(Inner {
                root: root.to_path_buf(),
                source: source.into(),
                snapshot_id: snapshot_id.into(),
                staging_dir: None,
                state: Mutex::new(State::new(tree)),
            }),
        })
    }

    pub(crate) fn error(&self) -> Option<ObjectFailure> {
        self.inner.state().error.clone()
    }

    /// Seals and writes the staging manifest. Returns the payload bytes this
    /// run added to the pool (surviving newly-written unique objects) — the
    /// caller's incremental disk-usage delta.
    pub(crate) fn finish(&self, cancel: &CancellationToken) -> Result<u64, ObjectFailure> {
        not_cancelled(cancel)?;
        let started = Instant::now();
        let staging_dir = self
            .inner
            .staging_dir
            .as_ref()
            .ok_or_else(|| "object staging directory is missing".to_string())?;
        let (mut manifest, written) = {
            let mut state = self.inner.state();
            if let Some(error) = &state.error {
                return Err(error.clone());
            }
            if state.open_writers != 0 {
                return Err(ObjectFailure::integrity(format!(
                    "cannot publish object manifest with {} open writers",
                    state.open_writers
                )));
            }
            let _ = state.tree.remove(PROTOCOL_DIR);
            let tree = std::mem::replace(&mut state.tree, Tree::new());
            let entries = tree.into_entries();
            let facts = inspect_manifest_entries(&entries, || cancel.is_cancelled())?;
            not_cancelled(cancel)?;
            let manifest = Manifest {
                version: VERSION,
                source_udid: self.inner.source.clone(),
                snapshot_id: self.inner.snapshot_id.clone(),
                created_unix: 0,
                size_bytes: facts.size_bytes,
                entries_sha256: facts.entries_sha256,
                entries,
            };
            (manifest, std::mem::take(&mut state.written))
        };
        let added_bytes = prune_orphan_objects(
            &self.inner.root,
            &self.inner.source,
            &manifest,
            &written,
            cancel,
        )?;
        not_cancelled(cancel)?;
        manifest.created_unix = unix_now();
        validate_manifest_header(&self.inner.source, &manifest)?;
        let temporary = staging_dir.join("manifest.json.tmp");
        let final_path = staging_dir.join("manifest.json");
        let file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)
            .map_err(|error| ObjectFailure::from_io("create staging object manifest", &error))?;
        let mut writer = BufWriter::new(CancelWriter {
            inner: file,
            cancel,
        });
        serde_json::to_writer(&mut writer, &manifest)
            .map_err(|error| ObjectFailure::from_serde("encode staging object manifest", &error))?;
        not_cancelled(cancel)?;
        writer
            .flush()
            .map_err(|error| ObjectFailure::from_io("flush staging object manifest", &error))?;
        let file = writer
            .into_inner()
            .map_err(|error| {
                ObjectFailure::from_io("finish staging object manifest", error.error())
            })?
            .inner;
        let manifest_bytes = file
            .metadata()
            .map_err(|error| ObjectFailure::from_io("inspect staging object manifest", &error))?
            .len();
        if manifest_bytes > MAX_MANIFEST_BYTES {
            return Err(ObjectFailure::integrity(format!(
                "staging object manifest is too large: {manifest_bytes} bytes"
            )));
        }
        file.sync_all()
            .map_err(|error| ObjectFailure::from_io("sync staging object manifest", &error))?;
        not_cancelled(cancel)?;
        drop(file);
        fs::rename(&temporary, &final_path)
            .map_err(|error| ObjectFailure::from_io("publish staging object manifest", &error))?;
        tracing::debug!(
            snapshot_id = %self.inner.snapshot_id,
            entries = manifest.entries.len(),
            manifest_bytes,
            added_bytes,
            elapsed_ms = started.elapsed().as_millis(),
            "object manifest finalized"
        );
        Ok(added_bytes)
    }

    pub(crate) fn free_disk_space(&self) -> u64 {
        // Reuse only idevice's portable filesystem-capacity query. All backup
        // reads and writes remain object-store operations.
        FsBackupDelegate.get_free_disk_space(&self.inner.root)
    }

    // A restore session has no staging directory; every mutation is refused.
    fn reject_write(&self) -> Result<&Path, IdeviceError> {
        self.inner
            .staging_dir
            .as_deref()
            .ok_or_else(|| internal_error("restore source is immutable"))
    }

    fn record_delegate_failure(&self, failure: ObjectFailure) -> IdeviceError {
        let detail = failure.detail.clone();
        self.inner.record_failure(failure);
        internal_error(detail)
    }

    // Per-item mb2 outcomes (a bad move, an occupied path): canon reports them to
    // the device and carries on, so they are logged and returned rather than
    // latched into the run-fatal slot that stored-data failures use.
    fn refuse(&self, operation: &str, detail: String) -> IdeviceError {
        tracing::warn!(operation, %detail, "object request refused");
        internal_error(detail)
    }

    pub(crate) fn open_file_read(&self, key: &str) -> Result<Box<dyn Read + Send>, IdeviceError> {
        // mobilebackup2 probes protocol files (notably Status.plist) even on a
        // first full backup, so a missing logical entry is a normal ENOENT. Once
        // the manifest lists an entry, object failures are sticky integrity ones.
        let file = {
            let state = self.inner.state();
            match state.tree.get(key) {
                None => return Err(IdeviceError::NotFound),
                Some(Node {
                    kind: NodeKind::File { object_ref, size },
                    ..
                }) => Ok((object_ref.clone(), *size)),
                Some(_) => Err(ObjectFailure::integrity(format!(
                    "object path {key:?} is not a file"
                ))),
            }
        };
        let (object_ref, size) = file.map_err(|failure| self.record_delegate_failure(failure))?;
        let result = (|| -> Result<Box<dyn Read + Send>, ObjectFailure> {
            let object_path =
                resolve_object_ref(&self.inner.root, &self.inner.source, &object_ref)?;
            let file = File::open(&object_path).map_err(|error| {
                ObjectFailure::integrity(format!("open object {}: {error}", object_path.display()))
            })?;
            let metadata = file.metadata().map_err(|error| {
                ObjectFailure::integrity(format!(
                    "inspect object {}: {error}",
                    object_path.display()
                ))
            })?;
            if !metadata.is_file() || metadata.len() != size as u64 {
                return Err(ObjectFailure::integrity(format!(
                    "object {} does not match manifest metadata",
                    object_path.display()
                )));
            }
            Ok(Box::new(HashVerifyReader::new(
                file,
                self.inner.clone(),
                size as u64,
                object_ref.clone(),
                format!("{key:?} ({object_ref})"),
            )) as Box<dyn Read + Send>)
        })();
        result.map_err(|failure| self.record_delegate_failure(failure))
    }

    pub(crate) fn create_file_write(
        &self,
        key: &str,
    ) -> Result<Box<dyn Write + Send>, IdeviceError> {
        let staging_dir = self.reject_write()?;
        let result = (|| -> Result<Box<dyn Write + Send>, ObjectFailure> {
            let temporary_dir = staging_dir.join("objects");
            let temporary = temporary_dir.join(format!("{}.tmp", uuid::Uuid::new_v4().simple()));
            let file = OpenOptions::new()
                .create_new(true)
                .write(true)
                .open(&temporary)
                .map_err(|error| ObjectFailure::from_io("create temporary object", &error))?;
            self.inner.state().open_writers += 1;
            Ok(Box::new(ObjectWriter {
                inner: self.inner.clone(),
                key: key.to_owned(),
                temporary,
                file: Some(file),
                size: 0,
                hasher: Sha256::new(),
                failed: false,
            }) as Box<dyn Write + Send>)
        })();
        result.map_err(|failure| self.record_delegate_failure(failure))
    }

    pub(crate) fn create_dir_all(&self, key: &str) -> Result<(), IdeviceError> {
        self.reject_write()?;
        self.inner
            .state()
            .tree
            .ensure_dir(key, unix_now())
            .map(|_| ())
            .map_err(|detail| self.refuse("create_dir_all", detail))
    }

    pub(crate) fn remove(&self, key: &str) -> Result<(), IdeviceError> {
        self.reject_write()?;
        if key.is_empty() {
            return Err(internal_error("cannot remove the object root"));
        }
        // Recursive by contract: the delegate's remove is documented to take a
        // directory with its subtree, matching the reference remove_dir_all.
        if self.inner.state().tree.remove(key).is_none() {
            return Err(self.refuse("remove", format!("object path {key:?} does not exist")));
        }
        Ok(())
    }

    pub(crate) fn rename(&self, from: &str, to: &str) -> Result<(), IdeviceError> {
        self.reject_write()?;
        if from.is_empty() || to.is_empty() || to.starts_with(&format!("{from}/")) {
            return Err(self.refuse("rename", "invalid object rename".into()));
        }
        self.inner
            .state()
            .tree
            .rename(from, to, unix_now())
            .map_err(|detail| self.refuse("rename", detail))
    }

    /// DLMessageCopyItem: a file overwrites its target, a missing source or type
    /// conflict is a silent skip. The recursive directory merge departs from canon
    /// on purpose — idevicebackup2 copies one level and leaves subdirectories empty.
    pub(crate) fn copy(&self, src: &str, dst: &str) -> Result<(), IdeviceError> {
        self.reject_write()?;
        if src.is_empty() || dst.is_empty() {
            return Err(self.refuse("copy", "invalid object copy".into()));
        }
        // A self-copy is a skip like any other unusable pair: canon has no way
        // to report a per-item failure, so it must not fail the whole backup.
        if dst == src || dst.starts_with(&format!("{src}/")) {
            return Ok(());
        }
        let mut state = self.inner.state();
        let Some(source) = state.tree.get(src).cloned() else {
            return Ok(());
        };
        state.tree.merge(dst, source, unix_now());
        Ok(())
    }

    pub(crate) fn exists(&self, key: &str) -> bool {
        self.inner.state().tree.get(key).is_some()
    }

    pub(crate) fn is_dir(&self, key: &str) -> bool {
        self.inner.state().tree.get(key).is_some_and(Node::is_dir)
    }

    pub(crate) fn list_dir(&self, key: &str) -> Result<Vec<DirEntryInfo>, IdeviceError> {
        let state = self.inner.state();
        let Some(children) = state.tree.get(key).and_then(Node::children) else {
            return Err(internal_error(format!(
                "object path {key:?} is not a directory"
            )));
        };
        let mut entries = Vec::new();
        for (name, child) in children {
            let (is_file, size) = match &child.kind {
                NodeKind::File { size, .. } => (true, (*size).max(0) as u64),
                NodeKind::Dir { .. } => (false, 0),
            };
            entries.push(DirEntryInfo {
                name: name.clone(),
                is_dir: !is_file,
                is_file,
                size,
                modified: system_time(child.modified_unix),
            });
        }
        Ok(entries)
    }
}

// Partitions `written` against the sealed manifest: objects it dropped are
// deleted, the ones it kept are totalled as the run's pool delta. Deduplicated
// objects are never in `written`.
fn prune_orphan_objects(
    root: &Path,
    source: &str,
    manifest: &Manifest,
    written: &BTreeSet<String>,
    cancel: &CancellationToken,
) -> Result<u64, ObjectFailure> {
    let mut referenced = BTreeSet::new();
    let mut added: u64 = 0;
    for entry in manifest
        .entries
        .values()
        .filter(|entry| entry.kind == "file")
    {
        validate_object_ref(&entry.object_ref)?;
        if referenced.insert(entry.object_ref.as_str()) && written.contains(&entry.object_ref) {
            added = added.saturating_add(entry.size.max(0) as u64);
        }
    }
    for object_ref in written {
        not_cancelled(cancel)?;
        if referenced.contains(object_ref.as_str()) {
            continue;
        }
        let path = resolve_object_ref(root, source, object_ref)?;
        match fs::remove_file(&path) {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => {
                return Err(ObjectFailure::from_io(
                    format!("remove superseded object {}", path.display()),
                    &error,
                ))
            }
        }
    }
    Ok(added)
}

fn internal_error(message: impl Into<String>) -> IdeviceError {
    IdeviceError::InternalError(message.into())
}

#[cfg(test)]
mod tests {
    use super::super::manifest::ManifestEntry;
    use super::super::{hex_lower, OBJECT_PREFIX_LEN};
    use super::*;
    use std::collections::BTreeMap;

    fn touch(path: &Path) {
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, b"x").unwrap();
    }

    fn test_session(name: &str) -> (PathBuf, ObjectSession) {
        let root =
            std::env::temp_dir().join(format!("airvault-{name}-{}", uuid::Uuid::new_v4().simple()));
        let source = "testudid01";
        let snapshot = "aaaaaaaa-0000-4000-8000-000000000001";
        let session = ObjectSession::backup(&root, source, snapshot, None).unwrap();
        (root, session)
    }

    fn write_object(session: &ObjectSession, key: &str, bytes: &[u8]) {
        let mut writer = session.create_file_write(key).unwrap();
        writer.write_all(bytes).unwrap();
        drop(writer);
    }

    fn publish_test_snapshot(root: &Path, source: &str, snapshot: &str) {
        let snapshots = root.join(format!("{source}/snapshots"));
        fs::create_dir_all(&snapshots).unwrap();
        fs::rename(
            root.join(format!("{source}/staging/{snapshot}/manifest.json")),
            snapshots.join(format!("{snapshot}.json")),
        )
        .unwrap();
    }

    fn entry_ref(session: &ObjectSession, key: &str) -> String {
        match &session.inner.state().tree.get(key).unwrap().kind {
            NodeKind::File { object_ref, .. } => object_ref.clone(),
            NodeKind::Dir { .. } => panic!("{key} is a directory"),
        }
    }

    #[test]
    fn write_records_and_read_verifies_content_hash() {
        let (root, session) = test_session("content-hash-write-test");
        let content = b"airvault content hash";
        write_object(&session, "data.bin", content);

        let object_ref = entry_ref(&session, "data.bin");
        assert_eq!(object_ref, hex_lower(&Sha256::digest(content)));
        let object_path = resolve_object_ref(&root, "testudid01", &object_ref).unwrap();
        assert_eq!(
            object_path,
            root.join(format!(
                "testudid01/objects/{}/{}",
                &object_ref[..OBJECT_PREFIX_LEN],
                object_ref
            ))
        );
        assert!(object_path.is_file());

        let mut reader = session.open_file_read("data.bin").unwrap();
        assert_eq!(reader.read(&mut []).unwrap(), 0);
        let mut restored = Vec::new();
        reader.read_to_end(&mut restored).unwrap();
        assert_eq!(restored, content);

        drop(reader);
        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn missing_logical_file_is_not_an_integrity_failure() {
        let (root, session) = test_session("missing-logical-file-test");

        let error = match session.open_file_read("Status.plist") {
            Ok(_) => panic!("missing logical file unexpectedly opened"),
            Err(error) => error,
        };
        assert!(matches!(error, IdeviceError::NotFound));
        assert!(session.error().is_none());

        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn missing_manifest_object_is_an_integrity_failure() {
        let (root, session) = test_session("missing-manifest-object-test");
        write_object(&session, "Status.plist", b"status");
        let object_ref = entry_ref(&session, "Status.plist");
        let object_path = resolve_object_ref(&root, "testudid01", &object_ref).unwrap();
        fs::remove_file(object_path).unwrap();

        let error = match session.open_file_read("Status.plist") {
            Ok(_) => panic!("missing manifest object unexpectedly opened"),
            Err(error) => error,
        };
        assert!(matches!(error, IdeviceError::InternalError(_)));
        let failure = session.error().unwrap();
        assert_eq!(failure.kind, super::super::ObjectFailureKind::Integrity);
        assert!(failure.detail.contains("open object"));

        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn identical_files_share_one_content_addressed_object() {
        let (root, session) = test_session("content-dedup-test");
        let content = b"same bytes";
        write_object(&session, "first.bin", content);
        write_object(&session, "second.bin", content);

        let first = entry_ref(&session, "first.bin");
        let second = entry_ref(&session, "second.bin");
        assert_eq!(first, second);
        assert_eq!(first, hex_lower(&Sha256::digest(content)));

        let objects = fs::read_dir(root.join(format!(
            "testudid01/objects/{}",
            &first[..OBJECT_PREFIX_LEN]
        )))
        .unwrap()
        .collect::<Result<Vec<_>, _>>()
        .unwrap();
        assert_eq!(objects.len(), 1);

        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn copy_directory_merge_clones_the_subtree() {
        let (root, session) = test_session("copy-directory-test");
        write_object(&session, "src/f1.bin", b"one");
        write_object(&session, "src/sub/f2.bin", b"two");
        write_object(&session, "dst/keep.bin", b"kept");

        session.copy("src", "dst").unwrap();

        assert_eq!(
            entry_ref(&session, "dst/f1.bin"),
            entry_ref(&session, "src/f1.bin")
        );
        assert_eq!(
            entry_ref(&session, "dst/sub/f2.bin"),
            entry_ref(&session, "src/sub/f2.bin")
        );
        assert!(
            session.exists("dst/keep.bin"),
            "merge must keep unrelated targets"
        );
        assert!(session.error().is_none());

        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    // "Snapshot.plist" sorts between "Snapshot" and "Snapshot/…" in byte order,
    // so every subtree walk must range from the "Snapshot/" prefix.
    #[test]
    fn subtree_walks_step_over_a_lexicographic_sibling() {
        let (root, session) = test_session("subtree-sibling-test");
        write_object(&session, "Snapshot/Manifest.db", b"inside");
        write_object(&session, "Snapshot.plist", b"sibling");

        session.copy("Snapshot", "Copied").unwrap();
        assert!(
            session.exists("Copied/Manifest.db"),
            "copy lost the subtree"
        );

        session.rename("Snapshot", "Moved").unwrap();
        assert!(
            session.exists("Moved/Manifest.db"),
            "rename lost the subtree"
        );
        assert!(!session.exists("Snapshot/Manifest.db"));

        session.remove("Moved").unwrap();
        assert!(
            !session.exists("Moved/Manifest.db"),
            "remove left an orphan"
        );
        assert!(session.exists("Snapshot.plist"), "the sibling must survive");

        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn copy_missing_source_is_a_silent_noop() {
        let (root, session) = test_session("copy-missing-source-test");

        session.copy("ghost", "clone").unwrap();

        assert!(!session.exists("clone"));
        assert!(session.error().is_none());

        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn directory_listings_follow_mutations() {
        let (root, session) = test_session("child-index-mutations-test");
        write_object(&session, "src/a.bin", b"a");
        write_object(&session, "src/nested/b.bin", b"b");

        let names = |key: &str| {
            session
                .list_dir(key)
                .unwrap()
                .into_iter()
                .map(|entry| entry.name)
                .collect::<Vec<_>>()
        };
        assert_eq!(names(""), ["src"]);
        assert_eq!(names("src"), ["a.bin", "nested"]);

        session.rename("src/nested", "moved").unwrap();
        assert_eq!(names(""), ["moved", "src"]);
        assert_eq!(names("moved"), ["b.bin"]);
        assert_eq!(names("src"), ["a.bin"]);

        session.copy("moved", "clone").unwrap();
        assert_eq!(names("clone"), ["b.bin"]);
        session.remove("moved").unwrap();
        assert_eq!(names(""), ["clone", "src"]);

        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn content_hash_rejects_same_size_corruption() {
        let (root, session) = test_session("content-hash-corruption-test");
        write_object(&session, "data.bin", b"correct bytes");

        let object_ref = entry_ref(&session, "data.bin");
        let object_path = resolve_object_ref(&root, "testudid01", &object_ref).unwrap();
        let mut corrupted = fs::read(&object_path).unwrap();
        corrupted[0] ^= 0xff;
        fs::write(&object_path, corrupted).unwrap();

        let mut reader = session.open_file_read("data.bin").unwrap();
        let error = reader.read_to_end(&mut Vec::new()).unwrap_err();
        assert_eq!(error.kind(), std::io::ErrorKind::InvalidData);
        assert!(error.to_string().contains("content hash mismatch"));
        assert_eq!(
            session.error().unwrap().kind,
            super::super::ObjectFailureKind::Integrity
        );
        let repeated = reader.read(&mut [0; 1]).unwrap_err();
        assert!(repeated.to_string().contains("content hash mismatch"));

        drop(reader);
        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn writing_duplicate_repairs_a_damaged_pool_object() {
        let (root, session) = test_session("content-hash-repair-test");
        let content = b"correct bytes";
        write_object(&session, "first.bin", content);

        let object_ref = entry_ref(&session, "first.bin");
        let object_path = resolve_object_ref(&root, "testudid01", &object_ref).unwrap();
        let mut corrupted = content.to_vec();
        corrupted[0] ^= 0xff;
        fs::write(&object_path, corrupted).unwrap();

        write_object(&session, "second.bin", content);

        assert_eq!(entry_ref(&session, "second.bin"), object_ref);
        assert_eq!(fs::read(&object_path).unwrap(), content);

        // A truncated object is damage the writer can prove and still holds the
        // bytes for, so it heals too instead of failing the run.
        fs::write(&object_path, &content[..4]).unwrap();
        write_object(&session, "third.bin", content);

        assert_eq!(fs::read(&object_path).unwrap(), content);
        assert!(session.error().is_none());

        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn finish_records_manifest_creation_time() {
        let (root, session) = test_session("completion-time-test");
        let before = unix_now();
        assert_eq!(session.finish(&CancellationToken::new()).unwrap(), 0);
        let after = unix_now();
        let manifest: Manifest = serde_json::from_slice(
            &fs::read(
                root.join("testudid01/staging/aaaaaaaa-0000-4000-8000-000000000001/manifest.json"),
            )
            .unwrap(),
        )
        .unwrap();
        assert!(manifest.created_unix >= before);
        assert!(manifest.created_unix <= after);

        drop(session);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn incremental_manifest_inherits_content_address() {
        let (root, base) = test_session("content-hash-inheritance-test");
        write_object(&base, "data.bin", b"inherited bytes");
        let expected = entry_ref(&base, "data.bin");
        // The delta counts exactly the bytes this run added to the pool.
        assert_eq!(
            base.finish(&CancellationToken::new()).unwrap(),
            b"inherited bytes".len() as u64
        );
        publish_test_snapshot(&root, "testudid01", "aaaaaaaa-0000-4000-8000-000000000001");
        drop(base);

        let incremental = ObjectSession::backup(
            &root,
            "testudid01",
            "bbbbbbbb-0000-4000-8000-000000000002",
            Some("aaaaaaaa-0000-4000-8000-000000000001"),
        )
        .unwrap();
        assert_eq!(entry_ref(&incremental, "data.bin"), expected);

        drop(incremental);
        let _ = fs::remove_dir_all(root);
    }

    // prune deletes objects this run wrote that the final manifest dropped
    // (orphans), keeps referenced ones, never touches inherited objects, and
    // totals only the kept ones as the run's pool delta.
    #[test]
    fn prune_removes_only_unreferenced_written_objects() {
        let root = std::env::temp_dir().join("airvault-prune-orphans-test");
        let _ = fs::remove_dir_all(&root);
        let source = "testudid01";
        let ref_referenced = "11".repeat(32);
        let ref_orphan = "22".repeat(32);
        let ref_inherited = "33".repeat(32);

        let referenced_path = resolve_object_ref(&root, source, &ref_referenced).unwrap();
        let orphan_path = resolve_object_ref(&root, source, &ref_orphan).unwrap();
        let inherited_path = resolve_object_ref(&root, source, &ref_inherited).unwrap();
        touch(&referenced_path);
        touch(&orphan_path);
        touch(&inherited_path);

        let mut entries = BTreeMap::new();
        entries.insert(
            "Manifest.db".to_string(),
            ManifestEntry {
                kind: "file".into(),
                object_ref: ref_referenced.clone(),
                size: 1,
                modified_unix: 0,
            },
        );
        // An unchanged file inherited from the previous snapshot.
        entries.insert(
            "old.txt".to_string(),
            ManifestEntry {
                kind: "file".into(),
                object_ref: ref_inherited.clone(),
                size: 1,
                modified_unix: 0,
            },
        );
        let manifest = Manifest {
            version: VERSION,
            source_udid: source.into(),
            snapshot_id: "aaaaaaaa-0000-4000-8000-000000000001".into(),
            created_unix: 1,
            size_bytes: 2,
            entries_sha256: String::new(),
            entries,
        };
        // The writer recorded both objects it wrote this run; only one survived.
        let written: BTreeSet<String> = [ref_referenced, ref_orphan].into_iter().collect();

        let added = prune_orphan_objects(
            &root,
            source,
            &manifest,
            &written,
            &CancellationToken::new(),
        )
        .unwrap();

        assert_eq!(added, 1, "only a written object the manifest kept counts");
        assert!(referenced_path.exists(), "referenced object must be kept");
        assert!(
            !orphan_path.exists(),
            "unreferenced written object must be removed"
        );
        assert!(
            inherited_path.exists(),
            "an inherited object must never be touched"
        );
        let _ = fs::remove_dir_all(&root);
    }
}
