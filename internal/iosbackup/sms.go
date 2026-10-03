package iosbackup

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const (
	mediaDomain      = "MediaDomain"
	messagesDatabase = "Library/SMS/sms.db"
	// Attachments sit in MediaDomain; sms.db records them as "~/Library/SMS/…".
	attachmentsRoot = "Library/SMS/"
)

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
			WHERE `+smsShown+`)
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

// smsShown picks, of a message m, what a chat shows: not a reaction nor a
// group event.
const smsShown = "COALESCE(m.associated_message_type, 0) = 0 AND COALESCE(m.item_type, 0) = 0"

func (c *Contents) smsPage(ctx context.Context, chatIDs []int64) (chatPage, error) {
	db, err := c.domainDatabase(ctx, homeDomain, messagesDatabase)
	if err != nil {
		return chatPage{}, err
	}
	const columns = `m.ROWID, COALESCE(m.text, ''), m.attributedBody, m.date, COALESCE(m.is_from_me, 0) != 0,
		COALESCE(h.id, ''), COALESCE(m.service, '')`
	joined := `SELECT j.chat_id, ` + columns + ` FROM chat_message_join j JOIN message m ON m.ROWID = j.message_id
		LEFT JOIN handle h ON h.ROWID = m.handle_id WHERE ` + smsShown
	const byDate = " ORDER BY j.message_date DESC, j.message_id DESC"
	// A message can be in more than one chat: every chat's has it in each, in a
	// row; a conversation of several shows it once, as its first chat's.
	query, args := joined+byDate, []any(nil)
	switch {
	case len(chatIDs) == 1:
		query, args = joined+" AND j.chat_id = ?"+byDate, []any{chatIDs[0]}
	case len(chatIDs) > 1:
		chats, ids := inList(chatIDs)
		query = `SELECT ?, ` + columns + ` FROM message m LEFT JOIN handle h ON h.ROWID = m.handle_id
			WHERE m.ROWID IN (SELECT message_id FROM chat_message_join WHERE chat_id IN (` + chats + `)) AND ` + smsShown + `
			ORDER BY m.date DESC, m.ROWID DESC`
		args = append([]any{chatIDs[0]}, ids...)
	}
	return chatPage{db: db, query: query, args: args,
		scan: func(rows *sql.Rows) (Found, error) {
			var found Found
			var text string
			var body []byte
			var date int64
			err := rows.Scan(&found.Chat, &found.ID, &text, &body, &date, &found.FromMe, &found.Sender, &found.Service)
			found.Text, found.Time = messageText(text, body), messageTime(date)
			if found.FromMe {
				found.Sender = ""
			}
			return found, err
		},
		complete: func(ctx context.Context, messages []Message) error { return c.smsAttachments(ctx, db, messages) },
	}, nil
}

// smsAttachments adds to messages the files they carry and show.
func (c *Contents) smsAttachments(ctx context.Context, db *sql.DB, messages []Message) error {
	index := make(map[int64]int, len(messages)) // ROWID -> position in messages
	ids := make([]int64, len(messages))
	for i, message := range messages {
		index[message.ID], ids[i] = i, message.ID
	}
	shown, args := inList(ids)
	for rows, err := range c.rows(ctx, db, `SELECT j.message_id, COALESCE(a.filename, ''), COALESCE(a.transfer_name, ''),
			COALESCE(a.total_bytes, 0)
		FROM message_attachment_join j JOIN attachment a ON a.ROWID = j.attachment_id
		WHERE j.message_id IN (`+shown+`) AND COALESCE(a.hide_attachment, 0) = 0 ORDER BY a.ROWID`, args...) {
		var id, size int64
		var filename, name string
		if err == nil {
			err = rows.Scan(&id, &filename, &name, &size)
		}
		if err != nil {
			return fmt.Errorf("read the attachments: %w", err)
		}
		// Older records name the file by its absolute path on the phone.
		filePath := strings.TrimPrefix(strings.TrimPrefix(filename, "~/"), "/var/mobile/")
		i := index[id]
		messages[i].Attachments = append(messages[i].Attachments, c.attachment(ComponentMessages, filePath, name, size))
	}
	return nil
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
