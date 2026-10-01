package objectstore

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// node is a directory when children is non-nil, a file otherwise.
type node struct {
	children     map[string]*node
	objectRef    string
	size         int64
	modifiedUnix int64
}

func newDir(modifiedUnix int64) *node {
	return &node{children: map[string]*node{}, modifiedUnix: modifiedUnix}
}

func (n *node) isDir() bool { return n.children != nil }

// tree walks paths structurally, so subtree operations never depend on how
// keys sort; the flat map is only the manifest's wire format.
type tree struct {
	root *node
}

func newTree() *tree { return &tree{root: newDir(0)} }

// treeFromEntries relies on validation: every parent is a directory that
// sorts before its children.
func treeFromEntries(entries map[string]manifestEntry) *tree {
	t := newTree()
	for _, key := range slices.Sorted(maps.Keys(entries)) {
		entry := entries[key]
		parent, name := splitKey(key)
		leaf := &node{objectRef: entry.ObjectRef, size: entry.Size, modifiedUnix: entry.ModifiedUnix}
		if entry.Kind == entryDirectory {
			leaf = newDir(entry.ModifiedUnix)
		}
		t.get(parent).children[name] = leaf
	}
	return t
}

func splitKey(key string) (parent, name string) {
	if parent, name, found := strings.CutLast(key, "/"); found {
		return parent, name
	}
	return "", key
}

func (t *tree) get(key string) *node {
	current := t.root
	for part := range strings.SplitSeq(key, "/") {
		if part == "" {
			continue
		}
		if !current.isDir() {
			return nil
		}
		if current = current.children[part]; current == nil {
			return nil
		}
	}
	return current
}

type Entry struct {
	Name     string
	Dir      bool
	Size     int64
	Modified time.Time // zero when the snapshot never recorded it
}

func (t *tree) list(key string) ([]Entry, error) {
	dir := t.get(key)
	if dir == nil || !dir.isDir() {
		return nil, fmt.Errorf("path %q is not a directory", key)
	}
	entries := make([]Entry, 0, len(dir.children))
	for name, child := range dir.children {
		entry := Entry{Name: name, Dir: child.isDir(), Size: child.size}
		if child.modifiedUnix > 0 {
			entry.Modified = time.Unix(child.modifiedUnix, 0)
		}
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.Name, b.Name) })
	return entries, nil
}

func (t *tree) ensureDir(key string, now int64) (*node, error) {
	current := t.root
	for part := range strings.SplitSeq(key, "/") {
		if part == "" {
			continue
		}
		child := current.children[part]
		if child == nil {
			child = newDir(now)
			current.children[part] = child
		}
		if !child.isDir() {
			return nil, fmt.Errorf("path %q: %q is not a directory", key, part)
		}
		current = child
	}
	return current, nil
}

// slot returns the directory that holds key's leaf, creating ancestors.
func (t *tree) slot(key string, now int64) (*node, string, error) {
	parent, name := splitKey(key)
	if name == "" {
		return nil, "", fmt.Errorf("path %q has no name", key)
	}
	dir, err := t.ensureDir(parent, now)
	return dir, name, err
}

func (t *tree) insertFile(key, objectRef string, size, now int64) error {
	dir, name, err := t.slot(key, now)
	if err != nil {
		return err
	}
	if existing := dir.children[name]; existing != nil && existing.isDir() {
		return fmt.Errorf("path %q is a directory", key)
	}
	dir.children[name] = &node{objectRef: objectRef, size: size, modifiedUnix: now}
	return nil
}

// rename follows rename(2): a directory replaces only an empty directory.
// Every refusal comes before anything moves.
func (t *tree) rename(from, to string, now int64) error {
	source := t.get(from)
	switch {
	case source == nil:
		return fmt.Errorf("path %q does not exist", from)
	case from == to:
		return nil
	case strings.HasPrefix(to, from+"/"):
		return fmt.Errorf("cannot move %q into itself", from)
	}
	if target := t.get(to); target != nil && !replaces(target, source) {
		return fmt.Errorf("path %q cannot be replaced", to)
	}
	dir, name, err := t.slot(to, now)
	if err != nil {
		return err
	}
	t.remove(from)
	dir.children[name] = source
	return nil
}

func replaces(target, source *node) bool {
	if target.isDir() {
		return source.isDir() && len(target.children) == 0
	}
	return !source.isDir()
}

func (t *tree) remove(key string) *node {
	parent, name := splitKey(key)
	dir := t.get(parent)
	if name == "" || dir == nil || !dir.isDir() {
		return nil
	}
	removed := dir.children[name]
	delete(dir.children, name)
	return removed
}

// merge overwrites files with a fresh time, merges directories and skips a
// file/directory conflict.
func (t *tree) merge(key string, source *node, now int64) {
	dir, name, err := t.slot(key, now)
	if err == nil {
		mergeChild(dir, name, source, now)
	}
}

func mergeChild(dir *node, name string, source *node, now int64) {
	target := dir.children[name]
	if !source.isDir() {
		if target == nil || !target.isDir() {
			dir.children[name] = &node{objectRef: source.objectRef, size: source.size, modifiedUnix: now}
		}
		return
	}
	if target == nil {
		target = newDir(now)
		dir.children[name] = target
	}
	if !target.isDir() {
		return
	}
	for childName, child := range source.children {
		mergeChild(target, childName, child, now)
	}
}

func (t *tree) entries() map[string]manifestEntry {
	entries := map[string]manifestEntry{}
	var walk func(prefix string, dir *node)
	walk = func(prefix string, dir *node) {
		for name, child := range dir.children {
			key := prefix + name
			if child.isDir() {
				entries[key] = manifestEntry{Kind: entryDirectory, ModifiedUnix: child.modifiedUnix}
				walk(key+"/", child)
				continue
			}
			entries[key] = manifestEntry{Kind: entryFile, ObjectRef: child.objectRef, Size: child.size, ModifiedUnix: child.modifiedUnix}
		}
	}
	walk("", t.root)
	return entries
}
