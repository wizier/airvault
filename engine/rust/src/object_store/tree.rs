//! The in-memory snapshot tree. Paths are addressed structurally, so a subtree
//! operation never depends on how logical paths happen to sort as strings; the
//! flat `path -> entry` map exists only as the manifest's wire format.

use std::collections::BTreeMap;

use super::manifest::{EntryKind, ManifestEntry};

#[derive(Clone, Debug)]
pub(super) enum NodeKind {
    Dir { children: BTreeMap<String, Node> },
    File { object_ref: String, size: i64 },
}

#[derive(Clone, Debug)]
pub(super) struct Node {
    pub(super) kind: NodeKind,
    pub(super) modified_unix: i64,
}

impl Node {
    fn dir(modified_unix: i64) -> Self {
        Self {
            kind: NodeKind::Dir {
                children: BTreeMap::new(),
            },
            modified_unix,
        }
    }

    fn file(object_ref: String, size: i64, modified_unix: i64) -> Self {
        Self {
            kind: NodeKind::File { object_ref, size },
            modified_unix,
        }
    }

    // Holding children is what being a directory means; the exhaustive match
    // below is the one place a new NodeKind has to be accounted for.
    pub(super) fn is_dir(&self) -> bool {
        self.children().is_some()
    }

    pub(super) fn children(&self) -> Option<&BTreeMap<String, Node>> {
        match &self.kind {
            NodeKind::Dir { children } => Some(children),
            NodeKind::File { .. } => None,
        }
    }

    fn children_mut(&mut self) -> Option<&mut BTreeMap<String, Node>> {
        match &mut self.kind {
            NodeKind::Dir { children } => Some(children),
            NodeKind::File { .. } => None,
        }
    }
}

pub(super) struct Tree {
    root: Node,
}

// An empty key addresses the root; "" components cannot appear in a validated
// logical path, so filtering them keeps splitting total.
fn components(key: &str) -> impl Iterator<Item = &str> {
    key.split('/').filter(|part| !part.is_empty())
}

fn split_parent(key: &str) -> Option<(&str, &str)> {
    let (parent, name) = key.rsplit_once('/').unwrap_or(("", key));
    (!name.is_empty()).then_some((parent, name))
}

// rename(2) replacement rules: a file may replace a file, a directory only an
// empty directory. Anything else would drop a subtree the manifest still holds.
fn replaces(target: &Node, source: &Node) -> bool {
    match target.children() {
        Some(children) => source.is_dir() && children.is_empty(),
        None => !source.is_dir(),
    }
}

// Rebuilds the offending prefix only when a walk fails, so the walk itself
// never allocates a running path.
fn not_a_directory(key: &str, depth: usize) -> String {
    let path = components(key).take(depth).collect::<Vec<_>>().join("/");
    format!("object path {path:?} is not a directory")
}

impl Tree {
    pub(super) fn new() -> Self {
        Self { root: Node::dir(0) }
    }

    pub(super) fn get(&self, key: &str) -> Option<&Node> {
        let mut node = &self.root;
        for part in components(key) {
            node = node.children()?.get(part)?;
        }
        Some(node)
    }

    /// Creates `key` and every missing ancestor, yielding its children map.
    /// Fails on the first component already held by a file.
    pub(super) fn ensure_dir(
        &mut self,
        key: &str,
        modified_unix: i64,
    ) -> Result<&mut BTreeMap<String, Node>, String> {
        let mut node = &mut self.root;
        let mut depth = 0;
        for part in components(key) {
            node = node
                .children_mut()
                .ok_or_else(|| not_a_directory(key, depth))?
                .entry(part.to_owned())
                .or_insert_with(|| Node::dir(modified_unix));
            depth += 1;
        }
        node.children_mut()
            .ok_or_else(|| not_a_directory(key, depth))
    }

    /// The children map `key`'s leaf belongs in, with its leaf name. Missing
    /// ancestors are created; the root itself has no slot.
    fn slot<'t, 'k>(
        &'t mut self,
        key: &'k str,
        modified_unix: i64,
    ) -> Result<(&'t mut BTreeMap<String, Node>, &'k str), String> {
        let Some((parent, name)) = split_parent(key) else {
            return Err("cannot replace the object root".into());
        };
        Ok((self.ensure_dir(parent, modified_unix)?, name))
    }

    pub(super) fn insert_file(
        &mut self,
        key: &str,
        object_ref: String,
        size: i64,
        modified_unix: i64,
    ) -> Result<(), String> {
        let (children, name) = self.slot(key, modified_unix)?;
        if children.get(name).is_some_and(Node::is_dir) {
            return Err(format!("object path {key:?} is a directory"));
        }
        children.insert(name.to_owned(), Node::file(object_ref, size, modified_unix));
        Ok(())
    }

    /// Moves a node with its whole subtree. Every refusal is settled while the
    /// source is still attached, so no move is ever undone.
    pub(super) fn rename(
        &mut self,
        from: &str,
        to: &str,
        modified_unix: i64,
    ) -> Result<(), String> {
        let Some(source) = self.get(from) else {
            return Err(format!("object path {from:?} does not exist"));
        };
        if from == to {
            return Ok(());
        }
        if self.get(to).is_some_and(|target| !replaces(target, source)) {
            return Err(format!("object path {to:?} cannot be replaced"));
        }
        // The destination chain is built while the source is still attached, so
        // a bad target refuses here; only deletion happens afterward.
        self.slot(to, modified_unix)?;
        let moved = self.remove(from).expect("source was present above");
        let (children, name) = self.slot(to, modified_unix)?;
        children.insert(name.to_owned(), moved);
        Ok(())
    }

    /// Detaches a node with its whole subtree. The root is never removable.
    pub(super) fn remove(&mut self, key: &str) -> Option<Node> {
        let (parent, name) = split_parent(key)?;
        self.directory_mut(parent)?.remove(name)
    }

    fn directory_mut(&mut self, key: &str) -> Option<&mut BTreeMap<String, Node>> {
        let mut node = &mut self.root;
        for part in components(key) {
            node = node.children_mut()?.get_mut(part)?;
        }
        node.children_mut()
    }

    /// Clones `source` over `key`, merging directories and overwriting files —
    /// DLMessageCopyItem semantics. Files land with a fresh mtime because the
    /// canon rewrites them; a type conflict skips that item, like a failed open.
    pub(super) fn merge(&mut self, key: &str, source: Node, now: i64) {
        if let Ok((children, name)) = self.slot(key, now) {
            merge_child(children, name, source, now);
        }
    }

    pub(super) fn into_entries(self) -> BTreeMap<String, ManifestEntry> {
        let mut entries = BTreeMap::new();
        if let NodeKind::Dir { children } = self.root.kind {
            collect(children, &mut String::new(), &mut entries);
        }
        entries
    }

    /// Rebuilds the tree from a validated manifest's flat wire map, where
    /// inspect_manifest_entries guarantees every parent is an earlier directory.
    pub(super) fn from_entries(entries: &BTreeMap<String, ManifestEntry>) -> Self {
        let mut tree = Tree::new();
        for (key, entry) in entries {
            let node = match entry.kind {
                EntryKind::Directory => Node::dir(entry.modified_unix),
                EntryKind::File => {
                    Node::file(entry.object_ref.clone(), entry.size, entry.modified_unix)
                }
            };
            let (parent, name) = split_parent(key).expect("validated keys are non-empty");
            tree.directory_mut(parent)
                .expect("validated parents are directories")
                .insert(name.to_owned(), node);
        }
        tree
    }
}

fn merge_child(target: &mut BTreeMap<String, Node>, name: &str, source: Node, now: i64) {
    match source.kind {
        NodeKind::File { object_ref, size } => {
            if target.get(name).is_some_and(Node::is_dir) {
                return;
            }
            target.insert(name.to_owned(), Node::file(object_ref, size, now));
        }
        NodeKind::Dir {
            children: source_children,
        } => {
            let target_node = target
                .entry(name.to_owned())
                .or_insert_with(|| Node::dir(now));
            let Some(target_children) = target_node.children_mut() else {
                return;
            };
            for (child_name, child) in source_children {
                merge_child(target_children, &child_name, child, now);
            }
        }
    }
}

fn collect(
    children: BTreeMap<String, Node>,
    path: &mut String,
    entries: &mut BTreeMap<String, ManifestEntry>,
) {
    for (name, child) in children {
        let restore = path.len();
        if !path.is_empty() {
            path.push('/');
        }
        path.push_str(&name);
        let modified_unix = child.modified_unix;
        let (kind, object_ref, size, grandchildren) = match child.kind {
            NodeKind::Dir { children } => (EntryKind::Directory, String::new(), 0, Some(children)),
            NodeKind::File { object_ref, size } => (EntryKind::File, object_ref, size, None),
        };
        entries.insert(
            path.clone(),
            ManifestEntry {
                kind,
                object_ref,
                size,
                modified_unix,
            },
        );
        if let Some(children) = grandchildren {
            collect(children, path, entries);
        }
        path.truncate(restore);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn file(tree: &mut Tree, key: &str) {
        tree.insert_file(key, "11".repeat(32), 1, 7).unwrap();
    }

    // The whole point of the tree: a sibling that sorts between a directory and
    // its children ("Snapshot.plist" vs "Snapshot/") cannot affect a subtree.
    #[test]
    fn subtree_operations_ignore_lexicographic_siblings() {
        let mut tree = Tree::new();
        file(&mut tree, "Snapshot/Manifest.db");
        file(&mut tree, "Snapshot.plist");

        tree.rename("Snapshot", "Moved", 7).unwrap();

        assert!(tree.get("Snapshot/Manifest.db").is_none());
        assert!(tree.get("Snapshot.plist").is_some(), "sibling survives");
        assert!(tree.get("Moved/Manifest.db").is_some(), "subtree follows");
    }

    // rename(2) parity: the object store must refuse what the filesystem refuses,
    // or a move silently drops files the manifest still lists.
    #[test]
    fn a_move_never_clobbers_a_populated_target() {
        let mut tree = Tree::new();
        file(&mut tree, "Snapshot/ab/new.bin");
        file(&mut tree, "ab/old.bin");

        assert!(tree.rename("Snapshot/ab", "ab", 9).is_err());

        assert!(tree.get("ab/old.bin").is_some(), "target subtree survives");
        assert!(tree.get("Snapshot/ab/new.bin").is_some(), "source survives");

        tree.remove("ab/old.bin").expect("target file detaches");
        tree.rename("Snapshot/ab", "ab", 9)
            .expect("an empty target is replaceable");
        assert!(tree.get("ab/new.bin").is_some());
    }

    // rename(2) parity: a path moved onto itself succeeds and changes nothing —
    // the from == to guard, without which replacing a non-empty dir with itself
    // would refuse.
    #[test]
    fn a_self_rename_is_a_successful_noop() {
        let mut tree = Tree::new();
        file(&mut tree, "d/child.bin");

        tree.rename("d", "d", 9).expect("self-rename must succeed");

        assert!(tree.get("d/child.bin").is_some());
    }

    #[test]
    fn a_rejected_rename_keeps_its_source() {
        let mut tree = Tree::new();
        file(&mut tree, "source.bin");
        file(&mut tree, "blocked");

        assert!(tree.rename("source.bin", "blocked/target.bin", 9).is_err());

        assert!(tree.get("source.bin").is_some(), "rename lost its source");
        assert!(tree.get("blocked/target.bin").is_none());
    }

    #[test]
    fn merge_clones_the_subtree_and_keeps_unrelated_targets() {
        let mut tree = Tree::new();
        file(&mut tree, "src/a.bin");
        file(&mut tree, "src/deep/b.bin");
        file(&mut tree, "dst/keep.bin");

        let source = tree.get("src").unwrap().clone();
        tree.merge("dst", source, 99);

        assert!(tree.get("dst/a.bin").is_some());
        assert!(tree.get("dst/deep/b.bin").is_some());
        assert!(tree.get("dst/keep.bin").is_some(), "merge keeps the target");
        assert_eq!(tree.get("dst/a.bin").unwrap().modified_unix, 99);
    }

    #[test]
    fn merge_silently_skips_file_directory_conflicts() {
        let mut tree = Tree::new();
        file(&mut tree, "source-file");
        file(&mut tree, "target-dir/keep.bin");
        let source_file = tree.get("source-file").unwrap().clone();

        tree.merge("target-dir", source_file, 99);

        assert!(tree.get("target-dir").unwrap().is_dir());
        assert!(tree.get("target-dir/keep.bin").is_some());

        file(&mut tree, "source-dir/child.bin");
        file(&mut tree, "target-file");
        let source_dir = tree.get("source-dir").unwrap().clone();

        tree.merge("target-file", source_dir, 99);

        assert!(!tree.get("target-file").unwrap().is_dir());
        assert!(tree.get("target-file/child.bin").is_none());
    }

    #[test]
    fn flattening_round_trips_through_the_wire_format() {
        let mut tree = Tree::new();
        file(&mut tree, "a/b/c.bin");
        tree.ensure_dir("a/empty", 5).unwrap();

        let entries = tree.into_entries();
        assert_eq!(
            entries.keys().collect::<Vec<_>>(),
            ["a", "a/b", "a/b/c.bin", "a/empty"]
        );
        let rebuilt = Tree::from_entries(&entries);
        assert_eq!(rebuilt.into_entries(), entries);
    }

    #[test]
    fn a_file_never_becomes_a_directory() {
        let mut tree = Tree::new();
        file(&mut tree, "a");
        assert!(tree.ensure_dir("a/b", 1).is_err());
        assert!(tree.insert_file("a/b", String::new(), 0, 1).is_err());
    }
}
