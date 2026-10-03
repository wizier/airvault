package iosbackup

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"howett.net/plist"
)

const callsDatabase = "Library/CallHistoryDB/CallHistory.storedata"

// ZCALLRECORD.ZSERVICE_PROVIDER values; an app that calls through CallKit
// records its bundle ID, lately behind its team's ID.
const (
	providerPhone    = "com.apple.Telephony"
	providerFaceTime = "com.apple.FaceTime"
)

// ZCALLRECORD.ZCALLTYPE of a FaceTime video call.
const callTypeVideo = 8

type Call struct {
	Address  string    `json:"address,omitempty"` // a phone number or an email; "" when withheld
	Name     string    `json:"name,omitempty"`    // from the contacts, else what the call recorded
	Time     time.Time `json:"time,omitzero"`     // none for a call a chat records
	Duration int       `json:"duration"`          // seconds
	Outgoing bool      `json:"outgoing,omitempty"`
	Answered bool      `json:"answered,omitempty"`
	Service  string    `json:"service,omitempty"` // "phone" | "facetime" | an app's bundle ID
	App      string    `json:"app,omitempty"`     // the app's name, when the backup holds it
	Video    bool      `json:"video,omitempty"`
	// The contact whose photo shows them, when it has one.
	ContactID int64 `json:"contactId,omitempty"`
}

// Calls reads the call history, newest first, with names from the contacts.
func (c *Contents) Calls(ctx context.Context) ([]Call, error) {
	db, err := c.domainDatabase(ctx, homeDomain, callsDatabase)
	if err != nil {
		return nil, err
	}
	contacts := c.contactsByAddress(ctx)
	apps := map[string]string{} // bundle ID -> name
	calls := []Call{}
	for rows, err := range c.rows(ctx, db, `SELECT COALESCE(CAST(ZADDRESS AS TEXT), ''), COALESCE(ZNAME, ''), ZDATE,
		ZDURATION, COALESCE(ZORIGINATED, 0) != 0, COALESCE(ZANSWERED, 0) != 0, COALESCE(ZSERVICE_PROVIDER, ''),
		COALESCE(ZCALLTYPE, 0) FROM ZCALLRECORD ORDER BY ZDATE DESC`) {
		var call Call
		var provider string
		var callType int
		var taken, duration sql.NullFloat64
		if err == nil {
			err = rows.Scan(&call.Address, &call.Name, &taken, &duration, &call.Outgoing, &call.Answered, &provider, &callType)
		}
		if err != nil {
			return nil, fmt.Errorf("read the call history: %w", err)
		}
		call.Time = coreDataTime(taken)
		call.Duration = int(math.Round(duration.Float64))
		contact := contacts[addressKey(call.Address)]
		call.Name, call.ContactID = cmp.Or(contact.title(), personName(call.Name)), contact.ContactID
		switch provider {
		case providerPhone, "":
			call.Service = "phone"
		case providerFaceTime:
			call.Service, call.Video = "facetime", callType == callTypeVideo
		default:
			call.Service = bundleID(provider)
			if _, ok := apps[call.Service]; !ok {
				apps[call.Service] = c.appName(call.Service)
			}
			call.App = apps[call.Service]
		}
		calls = append(calls, call)
	}
	return calls, nil
}

// bundleID is the bundle ID in an app's identifier, which can lead with its
// team's ID: "C67CF9S4VU.ph.telegra.Telegraph".
func bundleID(identifier string) string {
	team, id, ok := strings.Cut(identifier, ".")
	isTeam := len(team) == 10 && strings.Trim(team, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") == ""
	if ok && isTeam && strings.Contains(id, ".") {
		return id
	}
	return identifier
}

// appName is the name an App Store app the backup holds goes by on the phone,
// from its store data; "" for any other.
func (c *Contents) appName(bundleID string) string {
	data, _ := c.backup.Info.Applications[bundleID].Metadata.([]byte)
	var metadata struct {
		DisplayName string `plist:"bundleDisplayName"`
		ItemName    string `plist:"itemName"`
	}
	if _, err := plist.Unmarshal(data, &metadata); err != nil {
		return ""
	}
	return cmp.Or(metadata.DisplayName, metadata.ItemName)
}
