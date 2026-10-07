package iosbackup

import (
	"context"
	"io/fs"
	"slices"
	"strings"
	"time"
)

// homeDomain holds the system's own databases, such as the messages, the
// contacts and the calls.
const homeDomain = "HomeDomain"

// Component is a part of a backup a view opens, by the database it lives in.
type Component string

const (
	ComponentPhotos    Component = "photos"
	ComponentContacts  Component = "contacts"
	ComponentCalls     Component = "calls"
	ComponentMessages  Component = "messages"
	ComponentWhatsApp  Component = "whatsapp"
	ComponentNotes     Component = "notes"
	ComponentPasswords Component = "passwords"
	ComponentFiles     Component = "files"
)

// In the order the backup's menu shows them: what the owner made, their
// conversations, then their people and passwords.
var components = []struct {
	id           Component
	domain, path string
	folder       bool // path is a folder of files, not a database
}{
	{ComponentPhotos, cameraRoll, photosDatabase, false},
	{ComponentNotes, notesDomain, notesDatabase, false},
	{ComponentFiles, fileProviderDomain, fileProviderStorage, true},
	{ComponentMessages, homeDomain, messagesDatabase, false},
	{ComponentWhatsApp, whatsAppDomain, whatsAppDatabase, false},
	{ComponentCalls, homeDomain, callsDatabase, false},
	{ComponentContacts, homeDomain, contactsDatabase, false},
	{ComponentPasswords, keychainDomain, keychainBackup, false},
}

// Components lists the parts this backup holds, in a fixed order: those whose
// database it holds, and a folder of files when it holds any.
func (c *Contents) Components(ctx context.Context) ([]Component, error) {
	held := []Component{}
	for _, component := range components {
		// An unencrypted backup's keychain is sealed to the device it came from.
		if component.id == ComponentPasswords && c.keys == nil {
			continue
		}
		there := c.stored(component.domain, component.path)
		if component.folder {
			var err error
			if there, err = c.holdsFiles(ctx, component.domain, component.path); err != nil {
				return nil, err
			}
		}
		if there {
			held = append(held, component.id)
		}
	}
	return held, nil
}

// fileRoots are where each component keeps its files: a domain, the folder
// their paths start from, what every such path starts with, and what none
// does. The photo library's own files are those it lists.
var fileRoots = map[Component]struct {
	domain, folder string
	roots          []string
	except         string
}{
	ComponentMessages: {mediaDomain, "", []string{attachmentsRoot}, ""},
	ComponentWhatsApp: {whatsAppDomain, "", []string{whatsAppMedia, whatsAppProfiles}, ""},
	// The notes' layout varies across iOS versions; only their database is not theirs to serve.
	ComponentNotes: {notesDomain, "", []string{""}, notesDatabase},
	ComponentFiles: {fileProviderDomain, fileProviderStorage, []string{""}, ""},
}

// OpenFile opens a file of a component by its path, with when it was last
// modified; it reads nothing else of the backup.
func (c *Contents) OpenFile(ctx context.Context, component Component, filePath string) (Reader, time.Time, error) {
	if component == ComponentPhotos {
		library, err := c.Photos(ctx)
		if err != nil {
			return nil, time.Time{}, err
		}
		if !library.holds(filePath) {
			return nil, time.Time{}, fs.ErrNotExist
		}
		return c.openPath(ctx, cameraRoll, filePath)
	}
	domain, path, ok := componentFile(component, filePath)
	if !ok {
		return nil, time.Time{}, fs.ErrNotExist
	}
	return c.openPath(ctx, domain, path)
}

// componentFile is where a file of a component is: its domain and its path
// there; false for a path not among the component's.
func componentFile(component Component, filePath string) (domain, path string, ok bool) {
	where, ok := fileRoots[component]
	if !ok || where.except != "" && strings.HasPrefix(filePath, where.except) ||
		!slices.ContainsFunc(where.roots, func(root string) bool { return strings.HasPrefix(filePath, root) }) {
		return "", "", false
	}
	return where.domain, join(where.folder, filePath), true
}

// join is name in folder, as Manifest.db writes it; "" is either one's top.
// It does not clean the path: Manifest.db is looked up as written.
func join(folder, name string) string {
	if folder == "" || name == "" {
		return folder + name
	}
	return folder + "/" + name
}
