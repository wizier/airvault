package iosbackup

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"
)

const (
	whatsAppDomain   = "AppDomainGroup-group.net.whatsapp.WhatsApp.shared"
	whatsAppDatabase = "ChatStorage.sqlite"
	// Optional companions: WhatsApp's copy of the address book, the phone
	// numbers behind LIDs, and the call log.
	whatsAppContacts = "ContactsV2.sqlite"
	whatsAppLIDs     = "LID.sqlite"
	whatsAppCallLog  = "CallHistory.sqlite"
	// Media sit under Message/Media/ (ChatStorage records them from Media/ on),
	// profile pictures under Media/Profile/.
	whatsAppMedia    = "Message/Media/"
	whatsAppProfiles = "Media/Profile/"
)

// ZWACHATSESSION.ZSESSIONTYPE of conversations; broadcast lists, status
// updates, communities and channels are not.
const (
	waPerson = 0
	waGroup  = 1
)

// ZWAMESSAGE.ZMESSAGETYPE values.
const (
	waText          = 0
	waImage         = 1
	waVideo         = 2
	waContact       = 4
	waLocation      = 5
	waGroupEvent    = 6
	waLink          = 7
	waDocument      = 8
	waSystem        = 10
	waGIF           = 11
	waWaiting       = 12
	waDeleted       = 14
	waTimer         = 28
	waViewOncePhoto = 38
	waViewOnceVideo = 39
	waPoll          = 46
	waViewOnceVoice = 53
	waVideoNote     = 54
	waCall          = 59
)

// waSaid are the types of what someone said, for SQL: the above, audio (3)
// and stickers (15), but not events. waShown adds them; the rest, such as album
// links (66), hold nothing to show.
const (
	waSaid  = "0, 1, 2, 3, 4, 5, 7, 8, 11, 14, 15, 38, 39, 46, 53, 54, 59"
	waShown = waSaid + ", 6, 10, 12, 28"
)

// waEvents are the group (6) and system (10) events shown, by ZGROUPEVENTTYPE.
var waEvents = map[[2]int]string{
	{waGroupEvent, 1}: "renamed", {waGroupEvent, 56}: "renamed",
	{waGroupEvent, 2}: "added", {waGroupEvent, 50}: "added", {waGroupEvent, 7}: "removed",
	{waGroupEvent, 3}: "left", {waGroupEvent, 15}: "joined", {waGroupEvent, 23}: "joined",
	{waGroupEvent, 12}: "created", {waGroupEvent, 4}: "photo", {waGroupEvent, 5}: "photoRemoved",
	{waGroupEvent, 17}: "description", {waGroupEvent, 26}: "timer", {waGroupEvent, 9}: "number",
	{waSystem, 2}: "encrypted", {waSystem, 3}: "security", {waSystem, 36}: "security",
	{waSystem, 5}: "number", {waSystem, 6}: "number",
}

// waCallEvents are the system events of calls: missed ones (45 under Do Not
// Disturb, 21 and 22 in a group) and group video calls.
var waCallEvents = map[int]Call{
	1: {}, 45: {}, 21: {},
	4: {Video: true}, 46: {Video: true}, 22: {Video: true},
	40: {Video: true, Answered: true}, 41: {Video: true, Answered: true},
}

// whatsAppChats lists WhatsApp's conversations, the latest first: chats with
// a person or a group where someone said something, deleted ones left out.
// Its last such message dates a chat: the date a chat records can be a
// placeholder in the far future, and WhatsApp's own chat holds only events.
func (c *Contents) whatsAppChats(ctx context.Context) ([]Chat, error) {
	db, err := c.domainDatabase(ctx, whatsAppDomain, whatsAppDatabase)
	if err != nil {
		return nil, err
	}
	people := c.whatsAppPeople(ctx, db)
	// Members who left stay: their messages still need a name.
	members := map[int64][]Participant{}
	for rows, err := range c.rows(ctx, db, `SELECT ZCHATSESSION, COALESCE(ZMEMBERJID, ''),
		COALESCE(ZCONTACTNAME, ZFIRSTNAME, '') FROM ZWAGROUPMEMBER ORDER BY ZCHATSESSION, Z_PK`) {
		var chat int64
		var jid, name string
		if err == nil {
			err = rows.Scan(&chat, &jid, &name)
		}
		if err != nil {
			return nil, fmt.Errorf("read the WhatsApp groups: %w", err)
		}
		member := people.person(jid)
		member.Avatar = people.avatar(jid)
		if member.Name == "" && !isNumber(name) {
			member.Name = name
		}
		members[chat] = append(members[chat], member)
	}
	chats := []Chat{}
	byPerson := map[string]int{} // a person's chats under a number and a LID are one
	for rows, err := range c.rows(ctx, db, `SELECT s.Z_PK, COALESCE(s.ZCONTACTJID, ''), COALESCE(s.ZPARTNERNAME, ''),
			COALESCE(s.ZSESSIONTYPE, 0), m.ZMESSAGEDATE, m.ZMESSAGETYPE, COALESCE(m.ZTEXT, ''), COALESCE(i.ZTITLE, '')
		FROM ZWACHATSESSION s
		JOIN ZWAMESSAGE m ON m.Z_PK = (SELECT Z_PK FROM ZWAMESSAGE WHERE ZCHATSESSION = s.Z_PK
			AND ZMESSAGETYPE IN (`+waSaid+`) AND (ZMESSAGETYPE != 0 OR COALESCE(ZTEXT, '') != '')
			ORDER BY ZSORT DESC LIMIT 1)
		LEFT JOIN ZWAMEDIAITEM i ON i.Z_PK = m.ZMEDIAITEM
		WHERE COALESCE(s.ZSESSIONTYPE, 0) IN (?, ?) AND COALESCE(s.ZREMOVED, 0) = 0
		ORDER BY m.ZMESSAGEDATE DESC`, waPerson, waGroup) {
		var id int64
		var jid, partner, text, title string
		var sessionType, lastType int
		var last sql.NullFloat64
		if err == nil {
			err = rows.Scan(&id, &jid, &partner, &sessionType, &last, &lastType, &text, &title)
		}
		if err != nil {
			return nil, fmt.Errorf("read the WhatsApp chats: %w", err)
		}
		chat := Chat{IDs: []int64{id}, Last: coreDataTime(last), Avatar: people.avatar(jid)}
		switch lastType {
		case waText, waLink:
			chat.Snippet = messageText(text, nil)
		case waImage, waVideo, waGIF, waVideoNote, waDocument:
			chat.Snippet = messageText(cmp.Or(title, text), nil)
		}
		partner = strings.Trim(partner, "\u200e\u200f")
		if sessionType == waGroup {
			chat.Title, chat.Participants = cmp.Or(partner, jid), members[id]
			chats = append(chats, chat)
			continue
		}
		person := people.person(jid)
		if !isNumber(partner) {
			person.Name = partner // the name in the address book, as WhatsApp keeps it
		}
		if i, ok := byPerson[person.Address]; ok {
			chats[i].IDs = append(chats[i].IDs, id)
			continue
		}
		byPerson[person.Address] = len(chats)
		chat.Title, chat.Participants = cmp.Or(person.Name, person.Address), []Participant{person}
		chats = append(chats, chat)
	}
	return chats, nil
}

// waRow is a message row with the chat, member and media it points at.
type waRow struct {
	id                       int64
	text                     string
	date                     sql.NullFloat64
	fromMe                   bool
	kind, event, sessionType int
	member, from, chat       string // JIDs
	local, title, mime, card string
	size                     int64
	latitude, longitude      sql.NullFloat64
	metadata, receipt        []byte
}

// whatsAppMessages pages WhatsApp messages, those of the types shown.
func (c *Contents) whatsAppMessages(ctx context.Context, chatIDs []int64, offset, limit int) ([]Message, error) {
	db, err := c.domainDatabase(ctx, whatsAppDomain, whatsAppDatabase)
	if err != nil {
		return nil, err
	}
	people := c.whatsAppPeople(ctx, db)
	// A chat sorts by ZSORT, which an index serves; a person's chats under a
	// number and a LID together only by date.
	order := "m.ZSORT DESC"
	if len(chatIDs) > 1 {
		order = "m.ZMESSAGEDATE DESC, m.Z_PK DESC"
	}
	chats, args := inList(chatIDs)
	messages := []Message{}
	var calls []int // positions of the calls the log can complete
	var callChats []string
	for rows, err := range c.rows(ctx, db, `SELECT m.Z_PK, COALESCE(m.ZTEXT, ''), m.ZMESSAGEDATE,
			COALESCE(m.ZISFROMME, 0) != 0, COALESCE(m.ZMESSAGETYPE, 0), COALESCE(m.ZGROUPEVENTTYPE, 0),
			COALESCE(s.ZSESSIONTYPE, 0), COALESCE(g.ZMEMBERJID, ''), COALESCE(m.ZFROMJID, ''), COALESCE(s.ZCONTACTJID, ''),
			COALESCE(i.ZMEDIALOCALPATH, ''), COALESCE(i.ZTITLE, ''), COALESCE(i.ZVCARDSTRING, ''), COALESCE(i.ZVCARDNAME, ''),
			COALESCE(i.ZFILESIZE, 0), i.ZLATITUDE, i.ZLONGITUDE, i.ZMETADATA, mi.ZRECEIPTINFO
		FROM ZWAMESSAGE m JOIN ZWACHATSESSION s ON s.Z_PK = m.ZCHATSESSION
		LEFT JOIN ZWAGROUPMEMBER g ON g.Z_PK = m.ZGROUPMEMBER LEFT JOIN ZWAMEDIAITEM i ON i.Z_PK = m.ZMEDIAITEM
		LEFT JOIN ZWAMESSAGEINFO mi ON mi.Z_PK = m.ZMESSAGEINFO
		WHERE m.ZCHATSESSION IN (`+chats+`) AND COALESCE(m.ZMESSAGETYPE, 0) IN (`+waShown+`)
		ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, limit, offset)...) {
		var r waRow
		if err == nil {
			err = rows.Scan(&r.id, &r.text, &r.date, &r.fromMe, &r.kind, &r.event, &r.sessionType, &r.member, &r.from,
				&r.chat, &r.local, &r.title, &r.mime, &r.card, &r.size, &r.latitude, &r.longitude, &r.metadata, &r.receipt)
		}
		if err != nil {
			return nil, fmt.Errorf("read the WhatsApp messages: %w", err)
		}
		if r.kind == waCall {
			group := "" // the log names the group of a group call, nothing for one with a person
			if r.sessionType == waGroup {
				group = r.chat
			}
			calls, callChats = append(calls, len(messages)), append(callChats, group)
		}
		messages = append(messages, c.whatsAppMessage(r, people))
	}
	// Only the call log knows a video call from a voice one.
	if len(calls) > 0 {
		log := c.whatsAppCallLog(ctx)
		for n, i := range calls {
			call := messages[i].Call
			if logged, ok := log.find(messages[i].Time, call.Duration, callChats[n]); ok {
				call.Video, call.Duration = logged.video, cmp.Or(call.Duration, logged.duration)
				call.Answered = call.Duration > 0
			}
		}
	}
	return messages, nil
}

func (c *Contents) whatsAppMessage(r waRow, people *waPeople) Message {
	m := Message{ID: r.id, Time: coreDataTime(r.date), FromMe: r.fromMe, Service: "WhatsApp"}
	// Who wrote it: a group member, else the person the chat is with.
	author := r.member
	if author == "" && r.sessionType == waPerson && !r.fromMe {
		author = cmp.Or(r.from, r.chat)
	}
	if author != "" && !r.fromMe {
		m.Sender = people.person(author).Address
	}
	switch r.kind {
	case waCall:
		// Its length, in seconds, is in the media item's metadata: 0 for a call not taken.
		seconds, _ := protoVarint(protoField(protoField(r.metadata, 87), 1), 3)
		m.Kind, m.Call = "call", &Call{Duration: int(seconds), Outgoing: r.fromMe, Answered: seconds > 0}
	case waSystem, waGroupEvent, waTimer:
		code := waEvents[[2]int{r.kind, r.event}]
		if r.kind == waTimer {
			code = "timer"
		}
		if call, ok := waCallEvents[r.event]; ok && r.kind == waSystem {
			m.Kind, m.Call = "call", &call
		} else if code != "" {
			m.Kind, m.Event = "event", people.event(code, r, author)
		}
	case waDeleted:
		m.Kind = "deleted"
	case waWaiting:
		m.Kind = "waiting"
	case waViewOncePhoto:
		m.Kind = "viewOncePhoto"
	case waViewOnceVideo:
		m.Kind = "viewOnceVideo"
	case waViewOnceVoice:
		m.Kind = "viewOnceVoice"
	case waPoll:
		m.Kind, m.Text = "poll", string(protoField(protoField(r.receipt, 8), 2))
	case waContact:
		m.Kind, m.Text = "contact", strings.ReplaceAll(r.card, "_$!<Name-Separator>!$_", ", ")
	case waLocation:
		if r.latitude.Valid {
			m.Kind, m.Location = "location", &Location{Latitude: r.latitude.Float64, Longitude: r.longitude.Float64,
				Name: cmp.Or(r.title, r.card)}
		}
	default:
		m.Text = messageText(r.text, nil)
		if r.kind == waImage || r.kind == waVideo || r.kind == waGIF || r.kind == waVideoNote {
			m.Text = messageText(cmp.Or(r.title, r.text), nil) // the caption
		}
		if r.local != "" {
			name := ""
			if r.kind == waDocument {
				name = r.title
			}
			filePath := "Message/" + strings.TrimPrefix(strings.TrimPrefix(r.local, "/"), "Message/")
			m.Attachments = []Attachment{c.attachment(whatsAppDomain, filePath, name, r.size)}
		}
	}
	return m
}

// waPeople names WhatsApp accounts by JID, a phone number's or a LID's.
type waPeople struct {
	phones   map[string]string // LID JID -> phone JID
	names    map[string]string // JID -> name in WhatsApp's address book or LID records
	pushed   map[string]string // JID -> the name its owner set
	contacts map[string]string // the system contacts' names by addressKey
	avatars  map[string]string // JID's user part -> profile picture
}

// whatsAppPeople reads who is who once. Every source but ChatStorage is
// optional: someone it does not name shows as the number.
func (c *Contents) whatsAppPeople(ctx context.Context, db *sql.DB) *waPeople {
	c.whatsAppMu.Lock()
	defer c.whatsAppMu.Unlock()
	if c.whatsApp != nil {
		return c.whatsApp
	}
	ctx = context.WithoutCancel(ctx) // every request waiting on the lock needs it
	p := &waPeople{phones: map[string]string{}, names: map[string]string{}, pushed: map[string]string{},
		contacts: c.contactNames(ctx), avatars: map[string]string{}}
	phone := func(id string) string { return waJID(id, "s.whatsapp.net") }
	lid := func(id string) string { return waJID(id, "lid") }
	c.pairs(ctx, db, `SELECT ZJID, ZPUSHNAME FROM ZWAPROFILEPUSHNAME`, func(jid, name string) { p.pushed[jid] = name })
	c.pairs(ctx, db, `SELECT ZCONTACTJID, ZCONTACTIDENTIFIER FROM ZWACHATSESSION WHERE ZCONTACTJID LIKE '%@lid'`,
		func(jid, number string) { p.phones[jid] = phone(number) })
	c.pairs(ctx, db, `SELECT ZLID, ZPHONENUMBER FROM ZWAPHONENUMBERLIDPAIR`,
		func(id, number string) { p.phones[lid(id)] = phone(number) })
	if contacts, err := c.domainDatabase(ctx, whatsAppDomain, whatsAppContacts); err == nil {
		c.pairs(ctx, contacts, `SELECT ZWHATSAPPID, ZFULLNAME FROM ZWAADDRESSBOOKCONTACT`,
			func(id, name string) { p.names[phone(id)] = name })
		c.pairs(ctx, contacts, `SELECT ZLID, ZWHATSAPPID FROM ZWAADDRESSBOOKCONTACT`,
			func(id, number string) { p.phones[lid(id)] = phone(number) })
	}
	if lids, err := c.domainDatabase(ctx, whatsAppDomain, whatsAppLIDs); err == nil {
		c.pairs(ctx, lids, `SELECT ZLID, ZPHONENUMBER FROM ZWAPHONENUMBERLIDPAIR`,
			func(id, number string) { p.phones[lid(id)] = phone(number) })
		c.pairs(ctx, lids, `SELECT ZIDENTIFIER, ZPHONENUMBER FROM ZWAZACCOUNT`,
			func(id, number string) { p.phones[lid(id)] = phone(number) })
		c.pairs(ctx, lids, `SELECT ZIDENTIFIER, ZDISPLAYNAME FROM ZWAZACCOUNT`,
			func(id, name string) { p.names[lid(id)] = name })
	}
	if stored, err := c.storedPaths(ctx, whatsAppDomain, whatsAppProfiles); err == nil {
		for file := range stored {
			// <number or LID>-<timestamp>.jpg; the latest picture wins.
			base := path.Base(file)
			if dash := strings.LastIndex(base, "-"); dash > 0 && path.Ext(base) == ".jpg" {
				p.avatars[base[:dash]] = max(p.avatars[base[:dash]], file)
			}
		}
	}
	c.whatsApp = p
	return p
}

// pairs reads two text columns of rows that may not be there in every version
// of WhatsApp; empty values are skipped.
func (c *Contents) pairs(ctx context.Context, db *sql.DB, query string, each func(a, b string)) {
	for rows, err := range c.rows(ctx, db, query) {
		var a, b sql.NullString
		if err == nil {
			err = rows.Scan(&a, &b)
		}
		if err != nil {
			slog.DebugContext(ctx, "WhatsApp names unreadable", "error", err)
			return
		}
		if a.String != "" && b.String != "" {
			each(a.String, b.String)
		}
	}
}

// waJID completes a JID that may lack its server.
func waJID(id, server string) string {
	if strings.Contains(id, "@") {
		return id
	}
	return strings.TrimPrefix(id, "+") + "@" + server
}

// person is who a JID names: the number shown, and a name from the address
// books, else the one they set themselves, marked "~" as WhatsApp does.
func (p *waPeople) person(jid string) Participant {
	phone := cmp.Or(p.phones[jid], jid)
	person := Participant{Address: jidAddress(phone)}
	if strings.HasPrefix(person.Address, "+") {
		person.Name = p.contacts[addressKey(person.Address)]
	}
	person.Name = cmp.Or(person.Name, p.names[phone], p.names[jid])
	if pushed := cmp.Or(p.pushed[jid], p.pushed[phone]); person.Name == "" && pushed != "" {
		person.Name = "~" + pushed
	}
	return person
}

func (p *waPeople) avatar(jid string) string {
	for _, id := range []string{jid, p.phones[jid]} {
		if user, _, _ := strings.Cut(id, "@"); user != "" && p.avatars[user] != "" {
			return p.avatars[user]
		}
	}
	return ""
}

// event reads a group or system event. ZTEXT holds its parameter, not its
// words: a JID, "<who>;<JID>,<JID>", JSON, or seconds.
func (p *waPeople) event(code string, r waRow, author string) *ChatEvent {
	event := &ChatEvent{Code: code}
	actor := author
	switch code {
	case "renamed":
		var change struct {
			Subject    string `json:"subject"`
			NewSubject string `json:"new_subject"`
			Author     string `json:"author"`
		}
		event.Text = r.text
		if json.Unmarshal([]byte(r.text), &change) == nil {
			event.Text, actor = cmp.Or(change.NewSubject, change.Subject), cmp.Or(change.Author, actor)
		}
	case "added", "removed":
		who, list, several := strings.Cut(r.text, ";")
		targets := []string{r.member}
		if several {
			targets = strings.Split(list, ",")
		}
		actor = who
		for _, jid := range targets {
			if jid != "" {
				event.Targets = append(event.Targets, p.person(jid))
			}
		}
	case "timer":
		event.Text = r.text
		if seconds, ok := protoVarint(r.metadata, 36); ok && r.kind == waTimer {
			event.Text = fmt.Sprint(seconds)
		}
	}
	if actor != "" {
		person := p.person(actor)
		event.Actor = &person
	}
	return event
}

// isNumber reports a name that is just a phone number, as WhatsApp records
// one for someone not in the contacts.
func isNumber(name string) bool {
	return strings.Trim(name, "+0123456789 -()\u200e\u200f") == ""
}

// jidAddress is who a WhatsApp JID names: "+<number>" for a phone account,
// the JID itself otherwise.
func jidAddress(jid string) string {
	if number, ok := strings.CutSuffix(jid, "@s.whatsapp.net"); ok {
		return "+" + number
	}
	return jid
}

// waLoggedCall is a call log entry: what a chat's record of a call lacks.
type waLoggedCall struct {
	at       time.Time
	duration int
	video    bool
	group    string // the group's JID; "" for a call with one person
}

type waCallLog []waLoggedCall

func (c *Contents) whatsAppCallLog(ctx context.Context) waCallLog {
	db, err := c.domainDatabase(ctx, whatsAppDomain, whatsAppCallLog)
	if err != nil {
		if !errors.Is(err, ErrNotStored) {
			slog.WarnContext(ctx, "WhatsApp call log unreadable", "error", err)
		}
		return nil
	}
	var log waCallLog
	for rows, err := range c.rows(ctx, db, `SELECT e.ZDATE, COALESCE(e.ZDURATION, 0), COALESCE(a.ZVIDEO, 0) != 0,
			COALESCE(e.ZGROUPJIDSTRING, '')
		FROM ZWACDCALLEVENT e LEFT JOIN ZWAAGGREGATECALLEVENT a ON a.Z_PK = e.Z1CALLEVENTS`) {
		var date, duration sql.NullFloat64
		var call waLoggedCall
		if err == nil {
			err = rows.Scan(&date, &duration, &call.video, &call.group)
		}
		if err != nil {
			slog.WarnContext(ctx, "WhatsApp call log unreadable", "error", err)
			return nil
		}
		call.at, call.duration = coreDataTime(date), int(duration.Float64)
		log = append(log, call)
	}
	return log
}

// find is the logged call a chat recorded at: in the same group, or with one
// person, within the call's length and a minute.
func (log waCallLog) find(at time.Time, duration int, group string) (found waLoggedCall, ok bool) {
	for _, call := range log {
		gap := call.at.Sub(at).Abs()
		if call.group != group || gap > time.Duration(max(duration, call.duration))*time.Second+time.Minute {
			continue
		}
		if !ok || gap < found.at.Sub(at).Abs() {
			found, ok = call, true
		}
	}
	return found, ok
}
