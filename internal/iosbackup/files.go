package iosbackup

import (
	"cmp"
	"context"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// The Files app keeps what is On My iPhone in a folder of its own group.
const (
	fileProviderDomain  = "AppDomainGroup-group.com.apple.FileProvider.LocalStorage"
	fileProviderStorage = "File Provider Storage"
)

// Entry is a file or a folder a folder holds, as the backup listed it.
type Entry struct {
	Name     string
	Folder   bool
	Size     int64     // a file's
	Modified time.Time // zero when the backup never recorded it
	Missing  bool      // listed, but its content is not in the backup
}

// Folder lists a folder among a component's files, "" being its top, by name
// as a device's folder is.
func (c *Contents) Folder(ctx context.Context, component Component, folder string) ([]Entry, error) {
	domain, dir, ok := componentFile(component, folder)
	if !ok {
		return nil, fs.ErrNotExist
	}
	listed, err := c.children(ctx, domain, dir)
	if err != nil {
		return nil, err
	}
	entries := []Entry{}
	seen := map[string]bool{}
	// Only what OpenFile serves, so a component's own database stays out.
	serves := func(name string) bool { _, _, ok := componentFile(component, join(folder, name)); return ok }
	for _, file := range listed {
		name := path.Base(file.path)
		if !serves(name) {
			continue
		}
		seen[name] = true
		entries = append(entries, Entry{Name: name, Folder: file.folder, Size: file.size,
			Modified: file.modified, Missing: !file.folder && !c.holds(file)})
	}
	// Folders the backup records only through the files inside them.
	implied, err := c.descendantFolders(ctx, domain, dir)
	if err != nil {
		return nil, err
	}
	for name := range implied {
		if !seen[name] && serves(name) {
			entries = append(entries, Entry{Name: name, Folder: true})
		}
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Name, b.Name))
	})
	return entries, nil
}
