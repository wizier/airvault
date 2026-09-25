//! One backup/restore session over the store: the in-memory snapshot tree plus
//! CAS object I/O and the final sealed publish.

use std::collections::BTreeSet;
use std::fs::{self, File, OpenOptions};
use std::io::{BufWriter, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Instant;

use sha2::{Digest, Sha256};
use tokio_util::sync::CancellationToken;

use super::manifest::{
    inspect_manifest_entries, load_manifest, validate_manifest_header, EntryKind, Manifest,
};
use super::object::{HashVerifyReader, ObjectWriter};
use super::tree::{Node, NodeKind, Tree};
use super::{
    not_cancelled, reject_symlinks, relative_components, resolve_object_ref, unix_now,
    validate_snapshot_id, validate_source, ObjectFailure, MAX_MANIFEST_BYTES, PROTOCOL_DIR,
    VERSION,
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
        self.state().error.get_or_insert(failure);
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

/// One child of a listed directory; `size` is None for a subdirectory.
pub(crate) struct ListedEntry {
    pub(crate) name: String,
    pub(crate) size: Option<u64>,
    pub(crate) modified_unix: i64,
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

        let tree = match base_snapshot_id {
            Some(base_snapshot_id) => {
                Tree::from_entries(&load_manifest(root, source, base_snapshot_id)?.entries)
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
        let tree = Tree::from_entries(&load_manifest(root, source, snapshot_id)?.entries);
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
        let staging_dir = self.reject_write()?;
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

    pub(crate) fn root(&self) -> &Path {
        &self.inner.root
    }

    // A restore session has no staging directory; every mutation is refused.
    fn reject_write(&self) -> Result<&Path, String> {
        self.inner
            .staging_dir
            .as_deref()
            .ok_or_else(|| "restore source is immutable".to_string())
    }

    fn record_delegate_failure(&self, failure: ObjectFailure) -> String {
        let detail = failure.detail.clone();
        self.inner.record_failure(failure);
        detail
    }

    // Per-item mb2 outcomes (a bad move, an occupied path): canon reports them to
    // the device and carries on, so they are logged and returned rather than
    // latched into the run-fatal slot that stored-data failures use.
    fn refuse(&self, operation: &str, detail: String) -> String {
        tracing::warn!(operation, %detail, "object request refused");
        detail
    }

    /// None when no logical entry exists at `key`.
    pub(crate) fn open_file_read(&self, key: &str) -> Result<Option<Box<dyn Read + Send>>, String> {
        // mobilebackup2 probes protocol files (notably Status.plist) even on a
        // first full backup, so a missing logical entry is a normal ENOENT. Once
        // the manifest lists an entry, object failures are sticky integrity ones.
        let file = {
            let state = self.inner.state();
            match state.tree.get(key) {
                None => return Ok(None),
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
        let result = (|| -> Result<Option<Box<dyn Read + Send>>, ObjectFailure> {
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
            Ok(Some(Box::new(HashVerifyReader::new(
                file,
                self.inner.clone(),
                size as u64,
                object_ref.clone(),
                format!("{key:?} ({object_ref})"),
            )) as Box<dyn Read + Send>))
        })();
        result.map_err(|failure| self.record_delegate_failure(failure))
    }

    pub(crate) fn create_file_write(&self, key: &str) -> Result<Box<dyn Write + Send>, String> {
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

    pub(crate) fn create_dir_all(&self, key: &str) -> Result<(), String> {
        self.reject_write()?;
        self.inner
            .state()
            .tree
            .ensure_dir(key, unix_now())
            .map(|_| ())
            .map_err(|detail| self.refuse("create_dir_all", detail))
    }

    pub(crate) fn remove(&self, key: &str) -> Result<(), String> {
        self.reject_write()?;
        if key.is_empty() {
            return Err("cannot remove the object root".into());
        }
        // Recursive by contract: the delegate's remove takes a directory with
        // its subtree, matching the reference remove_dir_all. A missing path is
        // ordinary — the crate clears every destination before it writes.
        if self.inner.state().tree.remove(key).is_none() {
            return Err(format!("object path {key:?} does not exist"));
        }
        Ok(())
    }

    pub(crate) fn rename(&self, from: &str, to: &str) -> Result<(), String> {
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
    pub(crate) fn copy(&self, src: &str, dst: &str) -> Result<(), String> {
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

    pub(crate) fn list_dir(&self, key: &str) -> Result<Vec<ListedEntry>, String> {
        let state = self.inner.state();
        let Some(children) = state.tree.get(key).and_then(Node::children) else {
            return Err(format!("object path {key:?} is not a directory"));
        };
        let entries = children.iter().map(|(name, child)| ListedEntry {
            name: name.clone(),
            size: match &child.kind {
                NodeKind::File { size, .. } => Some((*size).max(0) as u64),
                NodeKind::Dir { .. } => None,
            },
            modified_unix: child.modified_unix,
        });
        Ok(entries.collect())
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
        .filter(|entry| entry.kind == EntryKind::File)
    {
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

#[cfg(test)]
mod tests {
    use super::super::{ObjectFailureKind, OBJECT_PREFIX_LEN};
    use super::*;

    const SOURCE: &str = "testudid01";
    const SNAPSHOT: &str = "aaaaaaaa-0000-4000-8000-000000000001";

    // Removes the store even when an assertion fails mid-test.
    struct TempRoot(PathBuf);

    impl Drop for TempRoot {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn test_session() -> (TempRoot, ObjectSession) {
        let root =
            std::env::temp_dir().join(format!("airvault-test-{}", uuid::Uuid::new_v4().simple()));
        let session = ObjectSession::backup(&root, SOURCE, SNAPSHOT, None).unwrap();
        (TempRoot(root), session)
    }

    fn write_object(session: &ObjectSession, key: &str, bytes: &[u8]) {
        let mut writer = session.create_file_write(key).unwrap();
        writer.write_all(bytes).unwrap();
    }

    fn entry_ref(session: &ObjectSession, key: &str) -> String {
        match &session.inner.state().tree.get(key).unwrap().kind {
            NodeKind::File { object_ref, .. } => object_ref.clone(),
            NodeKind::Dir { .. } => panic!("{key} is a directory"),
        }
    }

    fn object_path(session: &ObjectSession, key: &str) -> PathBuf {
        resolve_object_ref(session.root(), SOURCE, &entry_ref(session, key)).unwrap()
    }

    #[test]
    fn identical_writes_share_one_verified_object() {
        let (root, session) = test_session();
        let content = b"same bytes";
        write_object(&session, "first.bin", content);
        write_object(&session, "second.bin", content);

        let object_ref = entry_ref(&session, "first.bin");
        assert_eq!(object_ref, format!("{:x}", Sha256::digest(content)));
        assert_eq!(entry_ref(&session, "second.bin"), object_ref);
        let shard = root.0.join(format!(
            "{SOURCE}/objects/{}",
            &object_ref[..OBJECT_PREFIX_LEN]
        ));
        assert_eq!(object_path(&session, "second.bin"), shard.join(&object_ref));
        assert_eq!(fs::read_dir(&shard).unwrap().count(), 1);

        let mut reader = session.open_file_read("second.bin").unwrap().unwrap();
        assert_eq!(reader.read(&mut []).unwrap(), 0);
        let mut restored = Vec::new();
        reader.read_to_end(&mut restored).unwrap();
        assert_eq!(restored, content);
    }

    // mobilebackup2 probes files it never wrote, so only a listed entry whose
    // object is gone is an integrity failure.
    #[test]
    fn only_a_listed_entry_with_a_missing_object_is_an_integrity_failure() {
        let (_root, session) = test_session();
        assert!(session.open_file_read("Status.plist").unwrap().is_none());
        assert!(session.error().is_none());

        write_object(&session, "Status.plist", b"status");
        fs::remove_file(object_path(&session, "Status.plist")).unwrap();
        assert!(session.open_file_read("Status.plist").is_err());
        let failure = session.error().unwrap();
        assert_eq!(failure.kind, ObjectFailureKind::Integrity);
        assert!(failure.detail.contains("open object"));
    }

    #[test]
    fn copy_missing_source_is_a_silent_noop() {
        let (_root, session) = test_session();

        session.copy("ghost", "clone").unwrap();

        assert!(!session.exists("clone"));
        assert!(session.error().is_none());
    }

    #[test]
    fn directory_listings_follow_mutations() {
        let (_root, session) = test_session();
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
        assert_eq!(
            entry_ref(&session, "clone/b.bin"),
            entry_ref(&session, "moved/b.bin")
        );
        session.remove("moved").unwrap();
        assert_eq!(names(""), ["clone", "src"]);
    }

    #[test]
    fn content_hash_rejects_same_size_corruption() {
        let (_root, session) = test_session();
        write_object(&session, "data.bin", b"correct bytes");

        let object_path = object_path(&session, "data.bin");
        let mut corrupted = fs::read(&object_path).unwrap();
        corrupted[0] ^= 0xff;
        fs::write(&object_path, corrupted).unwrap();

        let mut reader = session.open_file_read("data.bin").unwrap().unwrap();
        let error = reader.read_to_end(&mut Vec::new()).unwrap_err();
        assert_eq!(error.kind(), std::io::ErrorKind::InvalidData);
        assert!(error.to_string().contains("content hash mismatch"));
        assert_eq!(session.error().unwrap().kind, ObjectFailureKind::Integrity);
        let repeated = reader.read(&mut [0; 1]).unwrap_err();
        assert!(repeated.to_string().contains("content hash mismatch"));
    }

    #[test]
    fn writing_duplicate_repairs_a_damaged_pool_object() {
        let (_root, session) = test_session();
        let content = b"correct bytes";
        write_object(&session, "first.bin", content);

        let object_ref = entry_ref(&session, "first.bin");
        let object_path = object_path(&session, "first.bin");
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
    }

    // finish deletes objects this run wrote that its manifest dropped, counts
    // only the kept ones as the pool delta, and never touches inherited objects.
    #[test]
    fn incremental_backup_inherits_objects_and_prunes_its_orphans() {
        let (root, base) = test_session();
        write_object(&base, "inherited.bin", b"inherited");
        let inherited_ref = entry_ref(&base, "inherited.bin");
        let before = unix_now();
        assert_eq!(base.finish(&CancellationToken::new()).unwrap(), 9);
        let snapshots = root.0.join(format!("{SOURCE}/snapshots"));
        fs::create_dir_all(&snapshots).unwrap();
        fs::rename(
            root.0
                .join(format!("{SOURCE}/staging/{SNAPSHOT}/manifest.json")),
            snapshots.join(format!("{SNAPSHOT}.json")),
        )
        .unwrap();
        let created = load_manifest(&root.0, SOURCE, SNAPSHOT)
            .unwrap()
            .created_unix;
        assert!((before..=unix_now()).contains(&created));

        let next_snapshot = "bbbbbbbb-0000-4000-8000-000000000002";
        let next = ObjectSession::backup(&root.0, SOURCE, next_snapshot, Some(SNAPSHOT)).unwrap();
        assert_eq!(entry_ref(&next, "inherited.bin"), inherited_ref);
        let inherited = object_path(&next, "inherited.bin");
        write_object(&next, "new.bin", b"draft");
        let orphan = object_path(&next, "new.bin");
        write_object(&next, "new.bin", b"final");
        let kept = object_path(&next, "new.bin");
        next.remove("inherited.bin").unwrap();

        assert_eq!(next.finish(&CancellationToken::new()).unwrap(), 5);
        assert!(
            !orphan.exists(),
            "a dropped object this run wrote is pruned"
        );
        assert!(kept.exists());
        assert!(inherited.exists(), "the base snapshot still needs it");
    }
}
