package iosbackup

import (
	"context"
	"errors"
	"io/fs"
)

// Component is a part of a backup a view opens, by the database it lives in.
type Component string

const (
	ComponentPhotos   Component = "photos"
	ComponentContacts Component = "contacts"
	ComponentCalls    Component = "calls"
	ComponentMessages Component = "messages"
)

var components = []struct {
	id           Component
	domain, path string
}{
	{ComponentPhotos, cameraRoll, photosDatabase},
	{ComponentMessages, homeDomain, messagesDatabase},
	{ComponentContacts, homeDomain, contactsDatabase},
	{ComponentCalls, homeDomain, callsDatabase},
}

// Components lists the parts this backup holds, in a fixed order.
func (c *Contents) Components(ctx context.Context) ([]Component, error) {
	held := []Component{}
	for _, component := range components {
		file, err := c.stat(ctx, component.domain, component.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, err
		case c.holds(file):
			held = append(held, component.id)
		}
	}
	return held, nil
}
