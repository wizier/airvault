package iosbackup

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// maxFound is as many messages as a search of an app finds, the latest.
const maxFound = 200

// Match is where a message a search of a conversation found stands in its pages.
type Match struct {
	ID     int64 `json:"id"`
	Offset int   `json:"offset"`
}

// Search finds the messages of an app that show query, whatever its case, the
// latest first.
func (c *Contents) Search(ctx context.Context, app Component, query string) ([]Found, error) {
	page, err := c.appPage(ctx, app, nil)
	if err != nil {
		return nil, err
	}
	query = fold(query)
	found := []Found{}
	var last int64 // a message in two chats comes once for each, in a row
	for message, err := range c.messages(ctx, page, 0, -1) {
		if err != nil {
			return nil, fmt.Errorf("search the messages: %w", err)
		}
		if message.ID == last {
			continue
		}
		last = message.ID
		if message.says(query) {
			if found = append(found, message); len(found) == maxFound {
				break
			}
		}
	}
	return found, nil
}

// Matches finds the messages of a conversation's chats that show query,
// whatever its case, the latest first.
func (c *Contents) Matches(ctx context.Context, app Component, chatIDs []int64, query string) ([]Match, error) {
	page, err := c.chatPage(ctx, app, chatIDs)
	if err != nil {
		return nil, err
	}
	query = fold(query)
	matches := []Match{}
	offset := 0
	for message, err := range c.messages(ctx, page, 0, -1) {
		if err != nil {
			return nil, fmt.Errorf("search the chat: %w", err)
		}
		if message.says(query) {
			matches = append(matches, Match{ID: message.ID, Offset: offset})
		}
		offset++
	}
	return matches, nil
}

// says reports a message that shows query: in its text, a place's name or a
// document's title.
func (m Message) says(query string) bool {
	shown := []string{m.Text}
	if m.Location != nil {
		shown = append(shown, m.Location.Name)
	}
	for _, attachment := range m.Attachments {
		if attachment.titled {
			shown = append(shown, attachment.Name)
		}
	}
	return slices.ContainsFunc(shown, func(text string) bool { return strings.Contains(fold(text), query) })
}

// fold is text as a search compares it: whatever its case, е and ё alike.
func fold(text string) string { return strings.ReplaceAll(strings.ToLower(text), "ё", "е") }
