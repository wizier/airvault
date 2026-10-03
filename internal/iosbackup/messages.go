package iosbackup

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

const (
	mediaDomain      = "MediaDomain"
	messagesDatabase = "Library/SMS/sms.db"
	// Attachments sit in MediaDomain; sms.db records them as "~/Library/SMS/…".
	attachmentsRoot = "Library/SMS/"
)

type Participant struct {
	Address string `json:"address"` // a phone number or an email
	Name    string `json:"name,omitempty"`
	Avatar  string `json:"avatar,omitempty"` // a picture among the files
	// The contact whose photo shows them, when it has one.
	ContactID int64 `json:"contactId,omitempty"`
}

// Chat is a conversation as Messages shows it: one per set of people, over
// iMessage, SMS and RCS alike, so it can span several sms.db chats.
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
	switch {
	case len(chatIDs) == 0:
		return []Message{}, nil
	case app == ComponentMessages:
		return c.smsMessages(ctx, chatIDs, offset, limit)
	case app == ComponentWhatsApp:
		return c.whatsAppMessages(ctx, chatIDs, offset, limit)
	}
	return nil, fs.ErrNotExist
}

// attachment is a file a message carries, missing unless its component can
// open it.
func (c *Contents) attachment(component Component, filePath, name string, size int64) Attachment {
	domain, ok := fileDomain(component, filePath)
	return Attachment{Path: filePath, Name: cmp.Or(name, path.Base(filePath)), Size: size,
		Missing: !ok || !c.stored(domain, filePath)}
}

// inList is "?, ?, …" for ids, with them as its query arguments.
func inList(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return "?" + strings.Repeat(", ?", len(ids)-1), args
}

// smsChats lists the Messages app's conversations, the latest first.
func (c *Contents) smsChats(ctx context.Context) ([]Chat, error) {
	db, err := c.domainDatabase(ctx, homeDomain, messagesDatabase)
	if err != nil {
		return nil, err
	}
	contacts := c.contactsByAddress(ctx)
	participants := map[int64][]Participant{}
	for rows, err := range c.rows(ctx, db,
		`SELECT j.chat_id, h.id FROM chat_handle_join j JOIN handle h ON h.ROWID = j.handle_id ORDER BY j.chat_id, h.ROWID`) {
		var chat int64
		var address string
		if err == nil {
			err = rows.Scan(&chat, &address)
		}
		if err != nil {
			return nil, fmt.Errorf("read the chats: %w", err)
		}
		contact := contacts[addressKey(address)]
		participants[chat] = append(participants[chat], Participant{Address: address, Name: contact.title(),
			ContactID: contact.ContactID})
	}
	chats := []Chat{}
	byPeople := map[string]int{} // peopleKey -> position in chats
	for rows, err := range c.rows(ctx, db, `WITH ranked AS (
			SELECT j.chat_id, j.message_id, j.message_date, COUNT(*) OVER (PARTITION BY j.chat_id) AS messages,
				ROW_NUMBER() OVER (PARTITION BY j.chat_id ORDER BY j.message_date DESC, j.message_id DESC) AS n
			FROM chat_message_join j JOIN message m ON m.ROWID = j.message_id
			WHERE COALESCE(m.associated_message_type, 0) = 0 AND COALESCE(m.item_type, 0) = 0)
		SELECT c.ROWID, COALESCE(c.display_name, ''), COALESCE(c.chat_identifier, ''), r.messages, r.message_date,
			COALESCE(m.text, ''), m.attributedBody
		FROM chat c JOIN ranked r ON r.chat_id = c.ROWID AND r.n = 1 JOIN message m ON m.ROWID = r.message_id
		ORDER BY r.message_date DESC`) {
		var id, last int64
		var name, identifier, text string
		var messages int
		var body []byte
		if err == nil {
			err = rows.Scan(&id, &name, &identifier, &messages, &last, &text, &body)
		}
		if err != nil {
			return nil, fmt.Errorf("read the chats: %w", err)
		}
		// Rows come latest first, so a conversation's first one has its last message.
		key := peopleKey(id, participants[id])
		i, ok := byPeople[key]
		if !ok {
			i = len(chats)
			byPeople[key] = i
			people := participants[id]
			chat := Chat{Title: cmp.Or(name, peopleTitle(people), identifier), Participants: people,
				Last: messageTime(last), Snippet: messageText(text, body)}
			if len(people) == 1 {
				chat.ContactID = people[0].ContactID
			}
			chats = append(chats, chat)
		}
		chats[i].IDs = append(chats[i].IDs, id)
		chats[i].Messages += messages
	}
	return chats, nil
}

// peopleTitle names a chat by who is in it.
func peopleTitle(people []Participant) string {
	names := make([]string, len(people))
	for i, p := range people {
		names[i] = cmp.Or(p.Name, p.Address)
	}
	return strings.Join(names, ", ")
}

// peopleKey joins one-to-one chats with the same person, however the address
// is written; a group stands alone, as two can share their members.
func peopleKey(id int64, people []Participant) string {
	if len(people) != 1 {
		return fmt.Sprint("#", id)
	}
	return addressKey(people[0].Address)
}

// smsMessages pages sms.db messages, without reactions and group events.
func (c *Contents) smsMessages(ctx context.Context, chatIDs []int64, offset, limit int) ([]Message, error) {
	db, err := c.domainDatabase(ctx, homeDomain, messagesDatabase)
	if err != nil {
		return nil, err
	}
	chats, args := inList(chatIDs)
	messages := []Message{}
	index := map[int64]int{} // ROWID -> position in messages
	for rows, err := range c.rows(ctx, db, `SELECT m.ROWID, COALESCE(m.text, ''), m.attributedBody, j.message_date,
			COALESCE(m.is_from_me, 0) != 0, COALESCE(h.id, ''), COALESCE(m.service, '')
		FROM chat_message_join j JOIN message m ON m.ROWID = j.message_id LEFT JOIN handle h ON h.ROWID = m.handle_id
		WHERE j.chat_id IN (`+chats+`) AND COALESCE(m.associated_message_type, 0) = 0 AND COALESCE(m.item_type, 0) = 0
		ORDER BY j.message_date DESC, j.message_id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...) {
		var message Message
		var text string
		var date int64
		var body []byte
		if err == nil {
			err = rows.Scan(&message.ID, &text, &body, &date, &message.FromMe, &message.Sender, &message.Service)
		}
		if err != nil {
			return nil, fmt.Errorf("read the messages: %w", err)
		}
		message.Text = messageText(text, body)
		message.Time = messageTime(date)
		if message.FromMe {
			message.Sender = ""
		}
		index[message.ID] = len(messages)
		messages = append(messages, message)
	}
	if len(messages) == 0 {
		return messages, nil
	}
	ids := make([]int64, len(messages))
	for i, message := range messages {
		ids[i] = message.ID
	}
	page, args := inList(ids)
	for rows, err := range c.rows(ctx, db, `SELECT j.message_id, COALESCE(a.filename, ''), COALESCE(a.transfer_name, ''),
			COALESCE(a.total_bytes, 0)
		FROM message_attachment_join j JOIN attachment a ON a.ROWID = j.attachment_id
		WHERE j.message_id IN (`+page+`) AND COALESCE(a.hide_attachment, 0) = 0 ORDER BY a.ROWID`, args...) {
		var id, size int64
		var filename, name string
		if err == nil {
			err = rows.Scan(&id, &filename, &name, &size)
		}
		if err != nil {
			return nil, fmt.Errorf("read the attachments: %w", err)
		}
		// Older records name the file by its absolute path on the phone.
		filePath := strings.TrimPrefix(strings.TrimPrefix(filename, "~/"), "/var/mobile/")
		i := index[id]
		messages[i].Attachments = append(messages[i].Attachments, c.attachment(ComponentMessages, filePath, name, size))
	}
	return messages, nil
}

// messageTime reads a Messages timestamp since 2001: nanoseconds, or seconds
// before iOS 11.
func messageTime(value int64) time.Time {
	switch {
	case value == 0:
		return time.Time{}
	case value < 1e11:
		return time.Unix(coreDataEpoch+value, 0).UTC()
	}
	return time.Unix(coreDataEpoch, value).UTC()
}

// messageText is a message's plain text. Since iOS 16 the text column is often
// empty and only attributedBody holds it; the placeholders attachments leave
// in it are dropped.
func messageText(text string, body []byte) string {
	if text == "" {
		text = attributedText(body)
	}
	return strings.TrimSpace(strings.ReplaceAll(text, "\uFFFC", ""))
}

// attributedText reads the string of an NSAttributedString archived as a
// typedstream: its UTF-8 bytes follow the NSString class name, a '+' and their
// length.
func attributedText(body []byte) string {
	_, rest, ok := bytes.Cut(body, []byte("NSString"))
	plus := bytes.IndexByte(rest, '+')
	if !ok || plus < 0 || plus > 8 {
		return ""
	}
	rest = rest[plus+1:]
	var length int
	switch {
	case len(rest) >= 1 && rest[0] < 0x80:
		length, rest = int(rest[0]), rest[1:]
	case len(rest) >= 3 && rest[0] == 0x81:
		length, rest = int(binary.LittleEndian.Uint16(rest[1:3])), rest[3:]
	case len(rest) >= 5 && rest[0] == 0x82:
		length, rest = int(binary.LittleEndian.Uint32(rest[1:5])), rest[5:]
	default:
		return ""
	}
	if length > len(rest) {
		return ""
	}
	return string(rest[:length])
}
