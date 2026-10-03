package iosbackup

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
)

const callsDatabase = "Library/CallHistoryDB/CallHistory.storedata"

// ZCALLRECORD.ZSERVICE_PROVIDER values; an app that calls through CallKit
// records its bundle ID.
const (
	providerPhone    = "com.apple.Telephony"
	providerFaceTime = "com.apple.FaceTime"
)

// ZCALLRECORD.ZCALLTYPE of a FaceTime video call.
const callTypeVideo = 8

type Call struct {
	Address  string    `json:"address,omitempty"` // a phone number or an email; "" when withheld
	Name     string    `json:"name,omitempty"`    // from the contacts, else what the call recorded
	Time     time.Time `json:"time"`
	Duration int       `json:"duration"` // seconds
	Outgoing bool      `json:"outgoing,omitempty"`
	Answered bool      `json:"answered,omitempty"`
	Service  string    `json:"service"` // "phone" | "facetime" | an app's bundle ID
	Video    bool      `json:"video,omitempty"`
}

// Calls reads the call history, newest first, with names from the contacts.
func (c *Contents) Calls(ctx context.Context) ([]Call, error) {
	db, err := c.domainDatabase(ctx, homeDomain, callsDatabase)
	if err != nil {
		return nil, err
	}
	names := c.contactNames(ctx)
	calls := []Call{}
	err = c.query(ctx, db, func(rows *sql.Rows) error {
		var call Call
		var provider string
		var callType int
		var taken, duration sql.NullFloat64
		if err := rows.Scan(&call.Address, &call.Name, &taken, &duration, &call.Outgoing, &call.Answered,
			&provider, &callType); err != nil {
			return err
		}
		call.Time = coreDataTime(taken)
		call.Duration = int(math.Round(duration.Float64))
		call.Name = cmp.Or(names[addressKey(call.Address)], call.Name)
		switch provider {
		case providerPhone, "":
			call.Service = "phone"
		case providerFaceTime:
			call.Service, call.Video = "facetime", callType == callTypeVideo
		default:
			call.Service = provider
		}
		calls = append(calls, call)
		return nil
	}, `SELECT COALESCE(CAST(ZADDRESS AS TEXT), ''), COALESCE(ZNAME, ''), ZDATE, ZDURATION,
		COALESCE(ZORIGINATED, 0) != 0, COALESCE(ZANSWERED, 0) != 0, COALESCE(ZSERVICE_PROVIDER, ''),
		COALESCE(ZCALLTYPE, 0) FROM ZCALLRECORD ORDER BY ZDATE DESC`)
	if err != nil {
		return nil, fmt.Errorf("read the call history: %w", err)
	}
	return calls, nil
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

// addressKey matches a phone number by its last ten digits, so +7 916…,
// 8 916… and 916… are one number; an email matches case-insensitively.
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
	return digits[max(len(digits)-10, 0):]
}
