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
	"slices"
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
	waAudio         = 3
	waContact       = 4
	waLocation      = 5
	waGroupEvent    = 6
	waLink          = 7
	waDocument      = 8
	waSystem        = 10
	waGIF           = 11
	waWaiting       = 12
	waDeleted       = 14
	waSticker       = 15
	waTimer         = 28
	waViewOncePhoto = 38
	waViewOnceVideo = 39
	waPoll          = 46
	waViewOnceVoice = 53
	waVideoNote     = 54
	waCall          = 59
)

// waMedia name the media kinds' files, for one the backup does not hold.
var waMedia = map[int]string{waImage: "Photo", waVideo: "Video", waAudio: "Audio", waDocument: "Document",
	waGIF: "GIF", waSticker: "Sticker", waVideoNote: "Video message"}

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

// waChatsSQL picks, of the ZWACHATSESSION s, the chats listed.
var waChatsSQL = fmt.Sprintf("COALESCE(s.ZSESSIONTYPE, 0) IN (%d, %d) AND COALESCE(s.ZREMOVED, 0) = 0", waPerson, waGroup)

// waSaid are the types of what someone said; the rest are events, or hold
// nothing to show, such as album links (66).
var waSaid = []int{waText, waImage, waVideo, waAudio, waContact, waLocation, waLink, waDocument, waGIF, waDeleted,
	waSticker, waViewOncePhoto, waViewOnceVideo, waPoll, waViewOnceVoice, waVideoNote, waCall}

// waSaidSQL picks, of the ZWAMESSAGE m, what someone said, a text not empty.
var waSaidSQL = fmt.Sprintf(`COALESCE(m.ZMESSAGETYPE, 0) IN (%s)
	AND (COALESCE(m.ZMESSAGETYPE, 0) NOT IN (%d, %d) OR TRIM(COALESCE(m.ZTEXT, '')) != '')`, sqlList(waSaid), waText, waLink)

// waShownSQL picks what whatsAppMessage shows: what someone said, and the
// events it tells of.
var waShownSQL = func() string {
	var events []string
	for event := range waEvents {
		events = append(events, fmt.Sprintf("(%d, %d)", event[0], event[1]))
	}
	for event := range waCallEvents {
		events = append(events, fmt.Sprintf("(%d, %d)", waSystem, event))
	}
	slices.Sort(events)
	return fmt.Sprintf(`(%s OR m.ZMESSAGETYPE IN (%d, %d) OR (m.ZMESSAGETYPE, COALESCE(m.ZGROUPEVENTTYPE, 0)) IN (VALUES %s))`,
		waSaidSQL, waWaiting, waTimer, sqlList(events))
}()

// whatsAppChats lists WhatsApp's conversations, the latest first: chats with
// a person or a group where someone said something, deleted ones left out.
// Its last such message dates a chat: the date a chat records can be a
// placeholder in the far future, and WhatsApp's own chat holds only events.
func (c *Contents) whatsAppChats(ctx context.Context) ([]Chat, error) {
	db, err := c.domainDatabase(ctx, whatsAppDomain, whatsAppDatabase)
	if err != nil {
		return nil, err
	}
	people, calls := c.whatsAppPeople(ctx, db), c.whatsAppCallLog(ctx)
	// Members who left stay: their messages still need a name.
	members := map[int64][]Participant{}
	for rows, err := range c.rows(ctx, db, `SELECT ZCHATSESSION, COALESCE(ZMEMBERJID, '')
		FROM ZWAGROUPMEMBER ORDER BY ZCHATSESSION, Z_PK`) {
		var chat int64
		var jid string
		if err == nil {
			err = rows.Scan(&chat, &jid)
		}
		if err != nil {
			return nil, fmt.Errorf("read the WhatsApp groups: %w", err)
		}
		members[chat] = append(members[chat], people.person(jid))
	}
	chats := []Chat{}
	byPerson := map[string]int{} // a person's chats under a number and a LID are one
	for rows, err := range c.rows(ctx, db, `SELECT s.Z_PK, COALESCE(s.ZPARTNERNAME, ''), `+waColumns+`
		FROM ZWACHATSESSION s
		JOIN ZWAMESSAGE m ON m.Z_PK = (SELECT m.Z_PK FROM ZWAMESSAGE m WHERE m.ZCHATSESSION = s.Z_PK AND `+waSaidSQL+`
			ORDER BY m.ZSORT DESC LIMIT 1) `+waJoins+`
		WHERE `+waChatsSQL+`
		ORDER BY m.ZMESSAGEDATE DESC`) {
		var id int64
		var partner string
		var r waRow
		if err == nil {
			err = rows.Scan(append([]any{&id, &partner}, r.fields()...)...)
		}
		if err != nil {
			return nil, fmt.Errorf("read the WhatsApp chats: %w", err)
		}
		last, jid := c.whatsAppMessage(r, people, calls), r.chat
		chat := Chat{IDs: []int64{id}, Last: last.Time, Snippet: last.Text}
		if chat.Snippet == "" && len(last.Attachments) > 0 && last.Attachments[0].titled {
			chat.Snippet = last.Attachments[0].Name // a document without a caption
		}
		if r.sessionType == waGroup {
			chat.Title, chat.Participants = cmp.Or(unmarked(partner), jid), members[id]
			chat.Avatar = people.avatar(jid)
			chats = append(chats, chat)
			continue
		}
		person := people.person(jid)
		if i, ok := byPerson[person.Address]; ok {
			chats[i].IDs = append(chats[i].IDs, id)
			continue
		}
		byPerson[person.Address] = len(chats)
		chat.Title, chat.Participants, chat.Avatar = cmp.Or(person.Name, person.Address), []Participant{person}, person.Avatar
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
	local, title, card       string
	size                     int64
	latitude, longitude      sql.NullFloat64
	metadata, receipt        []byte
}

// waColumns are what a waRow reads of a message m, with waJoins: its chat s,
// sender g, media i and info mi.
const (
	waColumns = `m.Z_PK, COALESCE(m.ZTEXT, ''), m.ZMESSAGEDATE, COALESCE(m.ZISFROMME, 0) != 0,
		COALESCE(m.ZMESSAGETYPE, 0), COALESCE(m.ZGROUPEVENTTYPE, 0), COALESCE(s.ZSESSIONTYPE, 0), COALESCE(g.ZMEMBERJID, ''),
		COALESCE(m.ZFROMJID, ''), COALESCE(s.ZCONTACTJID, ''), COALESCE(i.ZMEDIALOCALPATH, ''), COALESCE(i.ZTITLE, ''),
		COALESCE(i.ZVCARDNAME, ''), COALESCE(i.ZFILESIZE, 0), i.ZLATITUDE, i.ZLONGITUDE, i.ZMETADATA, mi.ZRECEIPTINFO`
	waJoins = `LEFT JOIN ZWAGROUPMEMBER g ON g.Z_PK = m.ZGROUPMEMBER LEFT JOIN ZWAMEDIAITEM i ON i.Z_PK = m.ZMEDIAITEM
		LEFT JOIN ZWAMESSAGEINFO mi ON mi.Z_PK = m.ZMESSAGEINFO`
)

func (r *waRow) fields() []any {
	return []any{&r.id, &r.text, &r.date, &r.fromMe, &r.kind, &r.event, &r.sessionType, &r.member, &r.from, &r.chat,
		&r.local, &r.title, &r.card, &r.size, &r.latitude, &r.longitude, &r.metadata, &r.receipt}
}

func (c *Contents) whatsAppPage(ctx context.Context, chatIDs []int64) (chatPage, error) {
	db, err := c.domainDatabase(ctx, whatsAppDomain, whatsAppDatabase)
	if err != nil {
		return chatPage{}, err
	}
	people, calls := c.whatsAppPeople(ctx, db), c.whatsAppCallLog(ctx)
	// A chat sorts by ZSORT, which an index serves; a person's chats under a
	// number and a LID together, and every chat listed, only by date.
	where, order, args := waChatsSQL, "m.ZMESSAGEDATE DESC, m.Z_PK DESC", []any(nil)
	if chatIDs != nil {
		var chats string
		chats, args = inList(chatIDs)
		where = "m.ZCHATSESSION IN (" + chats + ")"
		if len(chatIDs) == 1 {
			order = "m.ZSORT DESC, m.Z_PK DESC"
		}
	}
	return chatPage{db: db, args: args,
		query: `SELECT m.ZCHATSESSION, ` + waColumns + `
		FROM ZWAMESSAGE m JOIN ZWACHATSESSION s ON s.Z_PK = m.ZCHATSESSION ` + waJoins + `
		WHERE ` + where + ` AND ` + waShownSQL + ` ORDER BY ` + order,
		scan: func(rows *sql.Rows) (Found, error) {
			var found Found
			var r waRow
			err := rows.Scan(append([]any{&found.Chat}, r.fields()...)...)
			found.Message = c.whatsAppMessage(r, people, calls)
			return found, err
		},
	}, nil
}

func (c *Contents) whatsAppMessage(r waRow, people *waPeople, calls waCallLog) Message {
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
		call := Call{Duration: int(seconds), Outgoing: r.fromMe, Answered: seconds > 0}
		// Only the call log knows a video call from a voice one; it names the
		// group of a group call.
		group := ""
		if r.sessionType == waGroup {
			group = r.chat
		}
		if logged, ok := calls.find(m.Time, call.Duration, group); ok {
			call.Video, call.Duration = logged.video, cmp.Or(call.Duration, logged.duration)
			call.Answered = call.Duration > 0
		}
		m.Kind, m.Call = "call", &call
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
		m.Kind = "location"
		if r.latitude.Valid {
			m.Location = &Location{Latitude: r.latitude.Float64, Longitude: r.longitude.Float64, Name: cmp.Or(r.title, r.card)}
		}
	default:
		m.Text = strings.TrimSpace(r.text)
		filePath, name := "", waMedia[r.kind] // media the backup does not hold keeps its kind's name
		if r.local != "" {
			filePath, name = "Message/"+strings.TrimPrefix(strings.TrimPrefix(r.local, "/"), "Message/"), ""
		}
		// Media keeps its caption in its title. A document names its file in its
		// text, or without a caption may in its title alone.
		fileName := ""
		switch r.kind {
		case waImage, waVideo, waGIF, waVideoNote:
			m.Text = strings.TrimSpace(cmp.Or(r.title, r.text))
		case waDocument:
			fileName = cmp.Or(r.text, r.title)
			m.Text, name = "", cmp.Or(fileName, name)
			if r.title != fileName {
				m.Text = strings.TrimSpace(r.title)
			}
		}
		if filePath != "" || name != "" {
			attachment := c.attachment(ComponentWhatsApp, filePath, name, r.size)
			attachment.titled = fileName != ""
			m.Attachments = []Attachment{attachment}
		}
	}
	return m
}

// waPeople names WhatsApp accounts by JID, a phone number's or a LID's.
type waPeople struct {
	phones   map[string]string  // LID JID -> phone JID
	lids     map[string]string  // phone JID -> LID JID
	names    waNames            // JID -> name in WhatsApp's copy of the address book
	pushed   waNames            // JID -> the name its owner set
	contacts map[string]Contact // the system contacts by addressKey
	avatars  map[string]string  // JID's user part -> profile picture
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
	p := &waPeople{phones: map[string]string{}, lids: map[string]string{}, names: waNames{}, pushed: waNames{},
		contacts: c.contactsByAddress(ctx), avatars: map[string]string{}}
	phone := func(id string) string { return waJID(id, "s.whatsapp.net") }
	lid := func(id string) string { return waJID(id, "lid") }
	link := func(id, number string) { p.phones[lid(id)], p.lids[phone(number)] = phone(number), lid(id) }
	c.pairs(ctx, db, `SELECT ZJID, ZPUSHNAME FROM ZWAPROFILEPUSHNAME`, p.pushed.set)
	c.pairs(ctx, db, `SELECT ZCONTACTJID, ZCONTACTIDENTIFIER FROM ZWACHATSESSION WHERE ZCONTACTJID LIKE '%@lid'`, link)
	c.pairs(ctx, db, `SELECT ZLID, ZPHONENUMBER FROM ZWAPHONENUMBERLIDPAIR`, link)
	// The names a chat or a group recorded, the later sources' better.
	c.pairs(ctx, db, `SELECT ZMEMBERJID, COALESCE(ZCONTACTNAME, ZFIRSTNAME) FROM ZWAGROUPMEMBER`, p.names.set)
	c.pairs(ctx, db, `SELECT ZCONTACTJID, ZPARTNERNAME FROM ZWACHATSESSION WHERE COALESCE(ZSESSIONTYPE, 0) = 0`, p.names.set)
	if contacts, err := c.domainDatabase(ctx, whatsAppDomain, whatsAppContacts); err == nil {
		c.pairs(ctx, contacts, `SELECT ZWHATSAPPID, ZFULLNAME FROM ZWAADDRESSBOOKCONTACT`,
			func(id, name string) { p.names.set(phone(id), name) })
		c.pairs(ctx, contacts, `SELECT ZLID, ZWHATSAPPID FROM ZWAADDRESSBOOKCONTACT`, link)
	}
	if lids, err := c.domainDatabase(ctx, whatsAppDomain, whatsAppLIDs); err == nil {
		c.pairs(ctx, lids, `SELECT ZLID, ZPHONENUMBER FROM ZWAPHONENUMBERLIDPAIR`, link)
		c.pairs(ctx, lids, `SELECT ZIDENTIFIER, ZPHONENUMBER FROM ZWAZACCOUNT`, link)
		c.pairs(ctx, lids, `SELECT ZIDENTIFIER, ZDISPLAYNAME FROM ZWAZACCOUNT`,
			func(id, name string) { p.names.set(lid(id), name) })
	}
	if stored, err := c.storedPaths(ctx, whatsAppDomain, whatsAppProfiles); err == nil {
		// <number or LID>-<timestamp>.jpg, and .thumb for the small copy most
		// have alone: the latest picture wins, its large copy over the small.
		rank := func(file string) string { return strings.TrimSuffix(file, ".thumb") }
		for file := range stored {
			base, ext := path.Base(file), path.Ext(file)
			if dash := strings.LastIndex(base, "-"); dash > 0 && (ext == ".jpg" || ext == ".thumb") {
				if id := base[:dash]; rank(file) > rank(p.avatars[id]) {
					p.avatars[id] = file
				}
			}
		}
	}
	c.whatsApp = p
	return p
}

// waNames are the names apps recorded for JIDs, real names only.
type waNames map[string]string

func (n waNames) set(jid, recorded string) {
	if name := personName(recorded); name != "" {
		n[jid] = name
	}
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

// ids are an account's phone and LID JIDs, either jid itself when unknown.
func (p *waPeople) ids(jid string) (phone, lid string) {
	phone = cmp.Or(p.phones[jid], jid)
	return phone, cmp.Or(p.lids[phone], jid)
}

// person is who a JID names: the number shown, and a name from the address
// books, else the one they set themselves, marked "~" as WhatsApp does.
func (p *waPeople) person(jid string) Participant {
	phone, lid := p.ids(jid)
	person := Participant{Address: jidAddress(phone), Avatar: p.avatar(jid)}
	if strings.HasPrefix(person.Address, "+") {
		contact := p.contacts[addressKey(person.Address)]
		person.Name, person.ContactID = contact.title(), contact.ContactID
	}
	person.Name = cmp.Or(person.Name, p.names[phone], p.names[lid])
	if pushed := cmp.Or(p.pushed[phone], p.pushed[lid]); person.Name == "" && pushed != "" {
		person.Name = "~" + pushed
	}
	return person
}

// avatar is the profile picture of an account or a group.
func (p *waPeople) avatar(jid string) string {
	phone, lid := p.ids(jid)
	for _, id := range []string{phone, lid} {
		if user, _, _ := strings.Cut(id, "@"); p.avatars[user] != "" {
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

// whatsAppCallLog reads the call log; chats stand without it.
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
