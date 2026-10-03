package iosbackup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

const (
	homeDomain       = "HomeDomain"
	contactsDatabase = "Library/AddressBook/AddressBook.sqlitedb"
)

// ABMultiValue.property values.
const (
	propertyPhone = 3
	propertyEmail = 4
)

type Contact struct {
	Name         string         `json:"name"`
	Organization string         `json:"organization,omitempty"`
	JobTitle     string         `json:"jobTitle,omitempty"`
	Note         string         `json:"note,omitempty"`
	Phones       []LabeledValue `json:"phones,omitempty"`
	Emails       []LabeledValue `json:"emails,omitempty"`
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
	contacts := []Contact{}
	index := map[int64]int{} // ROWID -> position in contacts
	for rows, err := range c.rows(ctx, db, `SELECT ROWID, COALESCE(First, ''), COALESCE(Middle, ''), COALESCE(Last, ''),
		COALESCE(Organization, ''), COALESCE(JobTitle, ''), COALESCE(Note, '') FROM ABPerson ORDER BY ROWID`) {
		var id int64
		var first, middle, last string
		var contact Contact
		if err == nil {
			err = rows.Scan(&id, &first, &middle, &last, &contact.Organization, &contact.JobTitle, &contact.Note)
		}
		if err != nil {
			return nil, fmt.Errorf("read the address book: %w", err)
		}
		contact.Name = strings.Join(strings.Fields(first+" "+middle+" "+last), " ")
		index[id] = len(contacts)
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

// contactNames maps the contacts' numbers and emails by addressKey to their
// names. Calls stand without them.
func (c *Contents) contactNames(ctx context.Context) map[string]string {
	contacts, err := c.Contacts(ctx)
	if err != nil {
		if !errors.Is(err, ErrNotStored) {
			slog.WarnContext(ctx, "contacts unreadable for the call history", "error", err)
		}
		return nil
	}
	names := map[string]string{}
	for _, contact := range contacts {
		name := cmp.Or(contact.Name, contact.Organization)
		for _, value := range append(contact.Phones, contact.Emails...) {
			if key := addressKey(value.Value); key != "" && name != "" {
				names[key] = name
			}
		}
	}
	return names
}

// addressKey matches a phone number by its last nine digits, so a national
// form with its trunk prefix (8 916…, 044 123…) matches the international
// one (+7 916…, +41 44 123…); an email matches case-insensitively.
func addressKey(address string) string {
	if strings.Contains(address, "@") {
		return strings.ToLower(address)
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, address)
	return digits[max(len(digits)-9, 0):]
}
