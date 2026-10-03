package iosbackup

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"iter"
	"path"
	"slices"
	"time"
)

type Participant struct {
	Address string `json:"address"` // a phone number or an email
	Name    string `json:"name,omitempty"`
	Avatar  string `json:"avatar,omitempty"` // a picture among the files
	// The contact whose photo shows them, when it has one.
	ContactID int64 `json:"contactId,omitempty"`
}

// Chat is a conversation as its app shows it, which can span several of the
// app's own chats: one per person, over iMessage, SMS and RCS alike in
// Messages, under a number and a LID alike in WhatsApp.
type Chat struct {
	IDs          []int64       `json:"ids"`
	Title        string        `json:"title"` // the group's name, else who is in it
	Participants []Participant `json:"participants,omitempty"`
	Avatar       string        `json:"avatar,omitempty"`    // a picture among the files
	ContactID    int64         `json:"contactId,omitempty"` // whose photo shows a chat with one person
	Messages     int           `json:"messages,omitempty"`  // shown ones, when the app counts them
	Last         time.Time     `json:"last"`
	Snippet      string        `json:"snippet,omitempty"` // the last message's text
}

type Message struct {
	ID          int64        `json:"id"`
	Text        string       `json:"text,omitempty"`
	Time        time.Time    `json:"time"`
	FromMe      bool         `json:"fromMe,omitempty"`
	Sender      string       `json:"sender,omitempty"`  // the address an incoming one came from
	Service     string       `json:"service,omitempty"` // "iMessage" | "SMS" | "RCS" | "WhatsApp"
	Attachments []Attachment `json:"attachments,omitempty"`
	// Kind is "" for what was written; else "call", "event", "location",
	// "contact" (Text names it), "poll" (Text asks), "deleted", "waiting",
	// "viewOncePhoto", "viewOnceVideo" or "viewOnceVoice".
	Kind     string     `json:"kind,omitempty"`
	Call     *Call      `json:"call,omitempty"`
	Event    *ChatEvent `json:"event,omitempty"`
	Location *Location  `json:"location,omitempty"`
}

// ChatEvent is a line a chat shows about itself: Code is "renamed" (Text is
// the name), "added", "removed", "left", "joined", "created", "photo",
// "photoRemoved", "description", "timer" (Text is its seconds), "number",
// "encrypted" or "security". A nil Actor is the backup's owner.
type ChatEvent struct {
	Code    string        `json:"code"`
	Actor   *Participant  `json:"actor,omitempty"`
	Targets []Participant `json:"targets,omitempty"`
	Text    string        `json:"text,omitempty"`
}

type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Name      string  `json:"name,omitempty"`
}

// Attachment is a file a message or a note carries. A table or a link in a
// note has no file and only a name, a scanned document its pages.
type Attachment struct {
	Path    string       `json:"path,omitempty"` // in its component's domain
	Name    string       `json:"name"`
	Size    int64        `json:"size,omitempty"`
	Missing bool         `json:"missing,omitempty"` // a file the backup does not hold
	Pages   []Attachment `json:"pages,omitempty"`
	titled  bool         // its name is one a person gave it, such as a document's title
}

// attachment is a file a message carries, missing unless its component can
// open it.
func (c *Contents) attachment(component Component, filePath, name string, size int64) Attachment {
	domain, ok := fileDomain(component, filePath)
	return Attachment{Path: filePath, Name: cmp.Or(name, path.Base(filePath)), Size: size,
		Missing: !ok || !c.stored(domain, filePath)}
}

// Found is a message with the chat it is in: what a page reads, and a search
// finds.
type Found struct {
	Chat int64 `json:"chat"`
	Message
}

// Chats lists an app's conversations, the latest first.
func (c *Contents) Chats(ctx context.Context, app Component) ([]Chat, error) {
	switch app {
	case ComponentMessages:
		return c.smsChats(ctx)
	case ComponentWhatsApp:
		return c.whatsAppChats(ctx)
	}
	return nil, fs.ErrNotExist
}

// Messages pages a conversation's messages, the latest first; chatIDs are the
// app's chats it is made of.
func (c *Contents) Messages(ctx context.Context, app Component, chatIDs []int64, offset, limit int) ([]Message, error) {
	page, err := c.chatPage(ctx, app, chatIDs)
	if err != nil {
		return nil, err
	}
	messages := []Message{}
	for found, err := range c.messages(ctx, page, offset, limit) {
		if err != nil {
			return nil, fmt.Errorf("read the messages: %w", err)
		}
		messages = append(messages, found.Message)
	}
	if page.complete != nil && len(messages) > 0 {
		return messages, page.complete(ctx, messages)
	}
	return messages, nil
}

// chatPage is the messages an app shows of chats, the latest first, each with
// the chat it is in; a page of them reads alike for the chat and its search.
type chatPage struct {
	db    *sql.DB
	query string // SELECT … ORDER BY …, taking args
	args  []any
	scan  func(*sql.Rows) (Found, error)
	// complete adds to a page what its rows lack, such as attachments.
	complete func(ctx context.Context, messages []Message) error
}

// chatPage is the page of a conversation, its chats with chatIDs.
func (c *Contents) chatPage(ctx context.Context, app Component, chatIDs []int64) (chatPage, error) {
	if len(chatIDs) == 0 {
		return chatPage{}, fs.ErrNotExist
	}
	return c.appPage(ctx, app, chatIDs)
}

// appPage is the page of the chats with chatIDs, or of every chat an app lists
// when nil.
func (c *Contents) appPage(ctx context.Context, app Component, chatIDs []int64) (chatPage, error) {
	switch app {
	case ComponentMessages:
		return c.smsPage(ctx, chatIDs)
	case ComponentWhatsApp:
		return c.whatsAppPage(ctx, chatIDs)
	}
	return chatPage{}, fs.ErrNotExist
}

// messages reads limit of a page's messages from offset, -1 for all of them.
func (c *Contents) messages(ctx context.Context, page chatPage, offset, limit int) iter.Seq2[Found, error] {
	return func(yield func(Found, error) bool) {
		args := slices.Concat(page.args, []any{limit, offset})
		for rows, err := range c.rows(ctx, page.db, page.query+" LIMIT ? OFFSET ?", args...) {
			var found Found
			if err == nil {
				found, err = page.scan(rows)
			}
			if !yield(found, err) || err != nil {
				return
			}
		}
	}
}
