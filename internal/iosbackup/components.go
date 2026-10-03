package iosbackup

import (
	"context"
	"io/fs"
	"slices"
	"strings"
	"time"
)

// Component is a part of a backup a view opens, by the database it lives in.
type Component string

const (
	ComponentPhotos   Component = "photos"
	ComponentContacts Component = "contacts"
	ComponentCalls    Component = "calls"
	ComponentMessages Component = "messages"
	ComponentWhatsApp Component = "whatsapp"
	ComponentNotes    Component = "notes"
)

var components = []struct {
	id           Component
	domain, path string
}{
	{ComponentPhotos, cameraRoll, photosDatabase},
	{ComponentMessages, homeDomain, messagesDatabase},
	{ComponentWhatsApp, whatsAppDomain, whatsAppDatabase},
	{ComponentNotes, notesDomain, notesDatabase},
	{ComponentContacts, homeDomain, contactsDatabase},
	{ComponentCalls, homeDomain, callsDatabase},
}

// Components lists the parts this backup holds, in a fixed order.
func (c *Contents) Components() []Component {
	held := []Component{}
	for _, component := range components {
		if c.stored(component.domain, component.path) {
			held = append(held, component.id)
		}
	}
	return held
}

// fileRoots are where each component keeps the files its items carry: a
// domain, what every such path starts with, and what none does. The photo
// library's own files are those it lists.
var fileRoots = map[Component]struct {
	domain string
	roots  []string
	except string
}{
	ComponentMessages: {mediaDomain, []string{attachmentsRoot}, ""},
	ComponentWhatsApp: {whatsAppDomain, []string{whatsAppMedia, whatsAppProfiles}, ""},
	// The notes' layout varies across iOS versions; only their database is not theirs to serve.
	ComponentNotes: {notesDomain, []string{""}, notesDatabase},
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
	domain, ok := fileDomain(component, filePath)
	if !ok {
		return nil, time.Time{}, fs.ErrNotExist
	}
	return c.openPath(ctx, domain, filePath)
}

// fileDomain is the domain of a file among a component's, false for any other.
func fileDomain(component Component, filePath string) (string, bool) {
	where, ok := fileRoots[component]
	if !ok || where.except != "" && strings.HasPrefix(filePath, where.except) {
		return "", false
	}
	return where.domain, slices.ContainsFunc(where.roots, func(root string) bool { return strings.HasPrefix(filePath, root) })
}
