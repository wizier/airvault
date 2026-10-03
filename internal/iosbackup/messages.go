package iosbackup

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"slices"
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
}

// Chat is a conversation as Messages shows it: one per set of people, over
// iMessage, SMS and RCS alike, so it can span several sms.db chats.
type Chat struct {
	IDs          []int64       `json:"ids"`
	Title        string        `json:"title"` // the group's name, else who is in it
	Participants []Participant `json:"participants,omitempty"`
	Messages     int           `json:"messages"`
	Last         time.Time     `json:"last"`
	Snippet      string        `json:"snippet,omitempty"` // the last message's text
	name         string        // the group's own name
	identifier   string        // the latest chat's address or group ID
}

type Message struct {
	ID          int64        `json:"id"`
	Text        string       `json:"text,omitempty"`
	Time        time.Time    `json:"time"`
	FromMe      bool         `json:"fromMe,omitempty"`
	Sender      string       `json:"sender,omitempty"`  // the address an incoming one came from
	Service     string       `json:"service,omitempty"` // "iMessage" | "SMS" | "RCS"
	Attachments []Attachment `json:"attachments,omitempty"`
}

type Attachment struct {
	Path    string `json:"path,omitempty"` // in MediaDomain
	Name    string `json:"name"`
	Type    string `json:"type,omitempty"` // MIME
	Size    int64  `json:"size"`
	Missing bool   `json:"missing,omitempty"` // not in the backup: kept only in iCloud
}

// Chats lists the conversations, the latest first.
func (c *Contents) Chats(ctx context.Context) ([]Chat, error) {
	db, err := c.domainDatabase(ctx, homeDomain, messagesDatabase)
	if err != nil {
		return nil, err
	}
	names := c.contactNames(ctx)
	participants := map[int64][]Participant{}
	err = c.query(ctx, db, func(rows *sql.Rows) error {
		var chat int64
		var address string
		err := rows.Scan(&chat, &address)
		participants[chat] = append(participants[chat], Participant{Address: address, Name: names[addressKey(address)]})
		return err
	}, `SELECT j.chat_id, h.id FROM chat_handle_join j JOIN handle h ON h.ROWID = j.handle_id ORDER BY j.chat_id, h.ROWID`)
	if err != nil {
		return nil, fmt.Errorf("read the chats: %w", err)
	}
	chats := []Chat{}
	byPeople := map[string]int{} // peopleKey -> position in chats
	err = c.query(ctx, db, func(rows *sql.Rows) error {
		var id, last int64
		var name, identifier, text string
		var messages int
		var body []byte
		if err := rows.Scan(&id, &name, &identifier, &messages, &last, &text, &body); err != nil {
			return err
		}
		// Rows come latest first, so a conversation's first one has its last message.
		key := peopleKey(id, participants[id])
		i, ok := byPeople[key]
		if !ok {
			i = len(chats)
			byPeople[key] = i
			chats = append(chats, Chat{Participants: participants[id], Last: messageTime(last),
				Snippet: messageText(text, body), identifier: identifier})
		}
		chat := &chats[i]
		chat.IDs = append(chat.IDs, id)
		chat.Messages += messages
		chat.name = cmp.Or(chat.name, name)
		return nil
	}, `WITH ranked AS (
			SELECT chat_id, message_id, message_date, COUNT(*) OVER (PARTITION BY chat_id) AS messages,
				ROW_NUMBER() OVER (PARTITION BY chat_id ORDER BY message_date DESC, message_id DESC) AS n
			FROM chat_message_join)
		SELECT c.ROWID, COALESCE(c.display_name, ''), COALESCE(c.chat_identifier, ''), r.messages, r.message_date, COALESCE(m.text, ''), m.attributedBody
		FROM chat c JOIN ranked r ON r.chat_id = c.ROWID AND r.n = 1 JOIN message m ON m.ROWID = r.message_id
		ORDER BY r.message_date DESC`)
	if err != nil {
		return nil, fmt.Errorf("read the chats: %w", err)
	}
	for i := range chats {
		chat := &chats[i]
		people := make([]string, len(chat.Participants))
		for j, p := range chat.Participants {
			people[j] = cmp.Or(p.Name, p.Address)
		}
		chat.Title = cmp.Or(chat.name, strings.Join(people, ", "), chat.identifier)
	}
	return chats, nil
}

// peopleKey names who is in a chat, however their addresses are written; a
// chat with nobody recorded stands alone.
func peopleKey(id int64, people []Participant) string {
	keys := make([]string, len(people))
	for i, p := range people {
		keys[i] = addressKey(p.Address)
	}
	slices.Sort(keys)
	return cmp.Or(strings.Join(keys, "\n"), fmt.Sprint("#", id))
}

// Messages pages a conversation's messages, the latest first, without
// reactions and group events; chatIDs are its sms.db chats.
func (c *Contents) Messages(ctx context.Context, chatIDs []int64, offset, limit int) ([]Message, error) {
	messages := []Message{}
	if len(chatIDs) == 0 {
		return messages, nil
	}
	db, err := c.domainDatabase(ctx, homeDomain, messagesDatabase)
	if err != nil {
		return nil, err
	}
	page := `WITH page AS (
		SELECT j.message_id, j.message_date FROM chat_message_join j JOIN message m ON m.ROWID = j.message_id
		WHERE j.chat_id IN (?` + strings.Repeat(", ?", len(chatIDs)-1) + `)
			AND COALESCE(m.associated_message_type, 0) = 0 AND COALESCE(m.item_type, 0) = 0
		ORDER BY j.message_date DESC, j.message_id DESC LIMIT ? OFFSET ?)
	`
	args := []any{}
	for _, id := range chatIDs {
		args = append(args, id)
	}
	args = append(args, limit, offset)
	index := map[int64]int{} // ROWID -> position in messages
	err = c.query(ctx, db, func(rows *sql.Rows) error {
		var message Message
		var text string
		var date int64
		var body []byte
		if err := rows.Scan(&message.ID, &text, &body, &date, &message.FromMe, &message.Sender, &message.Service); err != nil {
			return err
		}
		message.Text = messageText(text, body)
		message.Time = messageTime(date)
		if message.FromMe {
			message.Sender = ""
		}
		index[message.ID] = len(messages)
		messages = append(messages, message)
		return nil
	}, page+`SELECT m.ROWID, COALESCE(m.text, ''), m.attributedBody, page.message_date, COALESCE(m.is_from_me, 0) != 0,
			COALESCE(h.id, ''), COALESCE(m.service, '')
		FROM page JOIN message m ON m.ROWID = page.message_id LEFT JOIN handle h ON h.ROWID = m.handle_id
		ORDER BY page.message_date DESC, page.message_id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the messages: %w", err)
	}
	err = c.query(ctx, db, func(rows *sql.Rows) error {
		var id int64
		var filename string
		var attachment Attachment
		if err := rows.Scan(&id, &filename, &attachment.Name, &attachment.Type, &attachment.Size); err != nil {
			return err
		}
		// Older records name the file by its absolute path on the phone.
		attachment.Path = strings.TrimPrefix(strings.TrimPrefix(filename, "~/"), "/var/mobile/")
		attachment.Name = cmp.Or(attachment.Name, path.Base(attachment.Path))
		_, stored := c.backup.FileSize(fileKey(mediaDomain, attachment.Path))
		attachment.Missing = attachment.Path == "" || !stored
		if i, ok := index[id]; ok {
			messages[i].Attachments = append(messages[i].Attachments, attachment)
		}
		return nil
	}, page+`SELECT page.message_id, COALESCE(a.filename, ''), COALESCE(a.transfer_name, ''), COALESCE(a.mime_type, ''),
			COALESCE(a.total_bytes, 0)
		FROM page JOIN message_attachment_join j ON j.message_id = page.message_id JOIN attachment a ON a.ROWID = j.attachment_id
		WHERE COALESCE(a.hide_attachment, 0) = 0 ORDER BY a.ROWID`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the attachments: %w", err)
	}
	return messages, nil
}

// OpenAttachment opens a message attachment by its path in MediaDomain.
func (c *Contents) OpenAttachment(ctx context.Context, attachmentPath string) (Reader, time.Time, error) {
	if !strings.HasPrefix(attachmentPath, attachmentsRoot) {
		return nil, time.Time{}, fs.ErrNotExist
	}
	return c.openPath(ctx, mediaDomain, attachmentPath)
}

// messageTime reads a Messages timestamp: nanoseconds since 2001.
func messageTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
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

// fileKey is where the snapshot keeps a file the backup lists: its file ID is
// the SHA-1 of "<domain>-<path>".
func fileKey(domain, filePath string) string {
	sum := sha1.Sum([]byte(domain + "-" + filePath))
	id := hex.EncodeToString(sum[:])
	return id[:2] + "/" + id
}
