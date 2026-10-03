package iosbackup

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"unicode"
)

const (
	contactsDatabase = "Library/AddressBook/AddressBook.sqlitedb"
	contactImages    = "Library/AddressBook/AddressBookImages.sqlitedb"
)

// ABMultiValue.property values.
const (
	propertyPhone = 3
	propertyEmail = 4
)

type Contact struct {
	ID           int64          `json:"id"`
	Name         string         `json:"name"`
	Organization string         `json:"organization,omitempty"`
	JobTitle     string         `json:"jobTitle,omitempty"`
	Note         string         `json:"note,omitempty"`
	Phones       []LabeledValue `json:"phones,omitempty"`
	Emails       []LabeledValue `json:"emails,omitempty"`
	ContactID    int64          `json:"contactId,omitempty"` // its own ID when ContactPhoto has its photo
}

type LabeledValue struct {
	Label string `json:"label,omitempty"`
	Value string `json:"value"`
}

// Contacts reads the address book, by name; nameless contacts come last.
func (c *Contents) Contacts(ctx context.Context) ([]Contact, error) {
	db, err := c.domainDatabase(ctx, homeDomain, contactsDatabase)
	if err != nil {
		return nil, err
	}
	photos := c.contactPhotos(ctx)
	contacts := []Contact{}
	index := map[int64]int{} // ROWID -> position in contacts
	for rows, err := range c.rows(ctx, db, `SELECT ROWID, COALESCE(First, ''), COALESCE(Middle, ''), COALESCE(Last, ''),
		COALESCE(Organization, ''), COALESCE(JobTitle, ''), COALESCE(Note, '') FROM ABPerson ORDER BY ROWID`) {
		var first, middle, last string
		var contact Contact
		if err == nil {
			err = rows.Scan(&contact.ID, &first, &middle, &last, &contact.Organization, &contact.JobTitle, &contact.Note)
		}
		if err != nil {
			return nil, fmt.Errorf("read the address book: %w", err)
		}
		contact.Name = strings.Join(strings.Fields(first+" "+middle+" "+last), " ")
		if photos[contact.ID] {
			contact.ContactID = contact.ID
		}
		index[contact.ID] = len(contacts)
		contacts = append(contacts, contact)
	}
	for rows, err := range c.rows(ctx, db, `SELECT v.record_id, v.property, COALESCE(l.value, ''), v.value FROM ABMultiValue v
		LEFT JOIN ABMultiValueLabel l ON l.ROWID = v.label
		WHERE v.property IN (?, ?) AND v.value IS NOT NULL ORDER BY v.UID`, propertyPhone, propertyEmail) {
		var id, property int64
		var value LabeledValue
		if err == nil {
			err = rows.Scan(&id, &property, &value.Label, &value.Value)
		}
		if err != nil {
			return nil, fmt.Errorf("read the address book: %w", err)
		}
		i, ok := index[id]
		if !ok {
			continue
		}
		value.Label = labelName(value.Label)
		if property == propertyPhone {
			contacts[i].Phones = append(contacts[i].Phones, value)
		} else {
			contacts[i].Emails = append(contacts[i].Emails, value)
		}
	}
	slices.SortStableFunc(contacts, func(a, b Contact) int {
		an, bn := strings.ToLower(cmp.Or(a.Name, a.Organization)), strings.ToLower(cmp.Or(b.Name, b.Organization))
		if (an == "") != (bn == "") {
			return cmp.Compare(bn, an) // the named one first
		}
		return cmp.Compare(an, bn)
	})
	return contacts, nil
}

// labelName turns a built-in label such as "_$!<Mobile>!$_" into "Mobile";
// custom labels are kept as typed.
func labelName(label string) string {
	if inner, ok := strings.CutPrefix(label, "_$!<"); ok {
		if inner, ok := strings.CutSuffix(inner, ">!$_"); ok {
			return inner
		}
	}
	return label
}

// title is what names a contact: its name, else its company.
func (c Contact) title() string { return cmp.Or(c.Name, c.Organization) }

// contactsByAddress maps the named contacts by addressKey of their numbers and
// emails. Calls and chats stand without them.
func (c *Contents) contactsByAddress(ctx context.Context) map[string]Contact {
	contacts, err := c.Contacts(ctx)
	if err != nil {
		if !errors.Is(err, ErrNotStored) {
			slog.WarnContext(ctx, "contacts unreadable for names", "error", err)
		}
		return nil
	}
	byAddress := map[string]Contact{}
	for _, contact := range contacts {
		for _, value := range append(contact.Phones, contact.Emails...) {
			if key := addressKey(value.Value); key != "" && contact.title() != "" {
				byAddress[key] = contact
			}
		}
	}
	return byAddress
}

// contactPhotos is the contacts the images database holds a photo of; the
// contacts stand without it.
func (c *Contents) contactPhotos(ctx context.Context) map[int64]bool {
	db, err := c.domainDatabase(ctx, homeDomain, contactImages)
	if err != nil {
		if !errors.Is(err, ErrNotStored) {
			slog.WarnContext(ctx, "contact photos unreadable", "error", err)
		}
		return nil
	}
	photos := map[int64]bool{}
	for rows, err := range c.rows(ctx, db, `SELECT record_id FROM ABThumbnailImage UNION SELECT record_id FROM ABFullSizeImage`) {
		var id int64
		if err == nil {
			err = rows.Scan(&id)
		}
		if err != nil {
			slog.WarnContext(ctx, "contact photos unreadable", "error", err)
			return nil
		}
		photos[id] = true
	}
	return photos
}

// ContactPhoto reads a contact's photo, the cropped thumbnail before the whole
// image; fs.ErrNotExist when it has none a browser shows.
func (c *Contents) ContactPhoto(ctx context.Context, id int64) ([]byte, error) {
	db, err := c.domainDatabase(ctx, homeDomain, contactImages)
	if err != nil {
		return nil, err
	}
	for rows, err := range c.rows(ctx, db, `SELECT data FROM ABThumbnailImage WHERE record_id = ?1
		UNION ALL SELECT data FROM ABFullSizeImage WHERE record_id = ?1`, id) {
		var data []byte
		if err == nil {
			err = rows.Scan(&data)
		}
		if err != nil {
			return nil, fmt.Errorf("read a contact photo: %w", err)
		}
		if image := photoImage(data); image != nil {
			return image, nil
		}
	}
	return nil, fs.ErrNotExist
}

// photoImage is the JPEG or PNG a contact photo's data holds, which some iOS
// versions put a byte or so before; nil for anything else, such as pixels.
func photoImage(data []byte) []byte {
	for i := range min(len(data), 16) {
		if rest := data[i:]; bytes.HasPrefix(rest, []byte("\xff\xd8\xff")) || bytes.HasPrefix(rest, []byte("\x89PNG")) {
			return rest
		}
	}
	return nil
}

// personName is a name an app recorded for someone, without the direction
// marks around it; "" when it is a phone number, however formatted.
func personName(recorded string) string {
	if name := unmarked(recorded); !isNumber(name) {
		return name
	}
	return ""
}

// isNumber reports a phone number, however written: digits, no letter.
func isNumber(text string) bool {
	return strings.ContainsFunc(text, unicode.IsDigit) && !strings.ContainsFunc(text, unicode.IsLetter)
}

// unmarked is text without the spaces and direction marks around it.
func unmarked(text string) string {
	return strings.TrimFunc(text, func(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Bidi_Control, r) })
}

// addressKey is the same for the addresses of one party, however written: a
// phone number's last nine digits, so a national form with its trunk prefix
// (8 916…, 044 123…) matches the international one (+7 916…, +41 44 123…);
// an email, a sender's name (MegaFon) or a short code, whatever its case.
func addressKey(address string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, address)
	if len(digits) < 7 || strings.Contains(address, "@") {
		return strings.ToLower(strings.TrimSpace(address))
	}
	return digits[max(len(digits)-9, 0):]
}
