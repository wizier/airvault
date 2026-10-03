package iosbackup

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
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
	err = c.query(ctx, db, func(rows *sql.Rows) error {
		var id int64
		var first, middle, last string
		var contact Contact
		if err := rows.Scan(&id, &first, &middle, &last, &contact.Organization, &contact.JobTitle, &contact.Note); err != nil {
			return err
		}
		contact.Name = strings.Join(strings.Fields(first+" "+middle+" "+last), " ")
		index[id] = len(contacts)
		contacts = append(contacts, contact)
		return nil
	}, `SELECT ROWID, COALESCE(First, ''), COALESCE(Middle, ''), COALESCE(Last, ''),
		COALESCE(Organization, ''), COALESCE(JobTitle, ''), COALESCE(Note, '') FROM ABPerson ORDER BY ROWID`)
	if err != nil {
		return nil, fmt.Errorf("read the address book: %w", err)
	}
	err = c.query(ctx, db, func(rows *sql.Rows) error {
		var id, property int64
		var value LabeledValue
		if err := rows.Scan(&id, &property, &value.Label, &value.Value); err != nil {
			return err
		}
		i, ok := index[id]
		if !ok {
			return nil
		}
		contact := &contacts[i]
		value.Label = labelName(value.Label)
		if property == propertyPhone {
			contact.Phones = append(contact.Phones, value)
		} else {
			contact.Emails = append(contact.Emails, value)
		}
		return nil
	}, `SELECT v.record_id, v.property, COALESCE(l.value, ''), v.value FROM ABMultiValue v
		LEFT JOIN ABMultiValueLabel l ON l.ROWID = v.label
		WHERE v.property IN (?, ?) AND v.value IS NOT NULL ORDER BY v.UID`, propertyPhone, propertyEmail)
	if err != nil {
		return nil, fmt.Errorf("read the address book: %w", err)
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
