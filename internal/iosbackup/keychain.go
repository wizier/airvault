package iosbackup

import (
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"
)

// The keychain is backed up as one plist under its own domain; only an
// encrypted backup includes it, and every saved password in it is wrapped
// with the backup keybag.
const (
	keychainDomain = "KeychainDomain"
	keychainBackup = "keychain-backup.plist"
	airPortService = "AirPort" // the service of a saved Wi-Fi network's password
	// The group of what Safari and AutoFill save: the Passwords app's own.
	safariGroup = "com.apple.cfnetwork"
	// The groups of the system's own keys, such as those iCloud Keychain syncs
	// with: no one's password.
	securityGroup = "com.apple.security"
)

// appleGroups name the Apple apps whose groups hold passwords people saved.
var appleGroups = map[string]string{safariGroup: "Safari", "com.apple.FileProviderUI": "Files"}

// Secret is a saved password the keychain holds, decrypted.
type Secret struct {
	ID       int    `json:"id"`
	Kind     string `json:"kind"`              // "wifi" | "app" | "web"
	Title    string `json:"title"`             // the network, the service or the site
	Account  string `json:"account,omitempty"` // the user name, when the item names one
	Password string `json:"password"`
	Protocol string `json:"protocol,omitempty"` // a site's URL scheme: "https", "smb", …
	Port     int    `json:"port,omitempty"`     // a site's, when the item names one
	Auth     string `json:"auth,omitempty"`     // a site's sign-in: "form", "basic", "digest", …
	App      string `json:"app,omitempty"`      // what saved it: its name, else its bundle ID or group
	BundleID string `json:"bundleId,omitempty"` // the app's, when the backup holds its icon
}

// Keychain lists the saved passwords the backup holds: Wi-Fi networks first,
// then the passwords of apps and sites.
func (c *Contents) Keychain(ctx context.Context) ([]Secret, error) {
	reader, _, err := c.openPath(ctx, keychainDomain, keychainBackup)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	type item struct {
		Data []byte `plist:"v_Data"`
	}
	var keychain struct {
		Generic  []item `plist:"genp"`
		Internet []item `plist:"inet"`
	}
	if err := decodePlist(io.NewSectionReader(reader, 0, reader.Size()), &keychain); err != nil {
		return nil, fmt.Errorf("decode keychain: %w", err)
	}
	secrets := []Secret{}
	// An item that will not decrypt or parse is skipped, not fatal, as a damaged
	// attachment is elsewhere; the rest of the keychain still reads. A whole
	// keychain decrypting to nothing shows up as one warning, not a flood.
	unreadable := 0
	collect := func(items []item, web bool) {
		for _, it := range items {
			plain, err := c.decryptKeychainItem(it.Data)
			if err != nil {
				unreadable++
				continue
			}
			if plain == nil {
				continue // non-migratory: readable only on the device it came from
			}
			attrs, err := parseKeychainAttributes(plain)
			if err != nil {
				unreadable++ // decrypted, but not the attributes this expects
				continue
			}
			if secret, ok := c.keychainSecret(attrs, web); ok {
				secrets = append(secrets, secret)
			}
		}
	}
	collect(keychain.Generic, false)
	collect(keychain.Internet, true)
	if unreadable > 0 {
		slog.WarnContext(ctx, "some keychain items could not be read", "count", unreadable)
	}
	// Wi-Fi first, then apps, then sites; each group by title.
	slices.SortStableFunc(secrets, func(a, b Secret) int {
		return cmp.Or(cmp.Compare(kindRank[a.Kind], kindRank[b.Kind]),
			cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)))
	})
	for i := range secrets {
		secrets[i].ID = i
	}
	return secrets, nil
}

var kindRank = map[string]int{"wifi": 0, "app": 1, "web": 2}

// A protocol is a four-character code, most of them a URL scheme already; so
// is a sign-in, "dflt" naming none.
var (
	protocolSchemes = map[string]string{"htps": "https", "imps": "imaps", "pops": "pop3s"}
	authTypes       = map[string]string{"http": "basic", "httd": "digest", "dflt": ""}
)

// keychainSecret reads a saved password from an item's attributes; false when
// the item carries no readable password, such as an app token or a key. What
// saved it is in its access group, "<team ID>.<bundle ID>" for an app.
func (c *Contents) keychainSecret(attrs keychainAttrs, web bool) (Secret, bool) {
	password := attrs.text("v_Data")
	if password == "" || !utf8.ValidString(password) {
		return Secret{}, false
	}
	group := bundleID(attrs.text("agrp"))
	if group == securityGroup || strings.HasPrefix(group, securityGroup+".") {
		return Secret{}, false
	}
	account, service := attrs.text("acct"), attrs.text("svce")
	if !web && service == airPortService {
		return Secret{Kind: "wifi", Title: account, Password: password}, true
	}
	// An app group, "group.<bundle ID>", serves as a keychain group too.
	owned := strings.TrimPrefix(group, "group.")
	app := groupOwner(owned, c.backup.Info.Applications)
	name := c.appName(app)
	secret := Secret{Kind: "app", Title: cmp.Or(name, service, account), Account: account, Password: password,
		App: cmp.Or(name, appleGroups[groupOwner(owned, appleGroups)], group)}
	if _, err := c.AppIcon(app); err == nil {
		secret.BundleID = app
	}
	if web {
		server, protocol, auth := attrs.text("srvr"), attrs.code("ptcl"), attrs.code("atyp")
		// An app that files its own data under its bundle ID names no site.
		if server != "" && (server == app || server == group) {
			server = cmp.Or(name, server)
		}
		if label, ok := authTypes[auth]; ok {
			auth = label
		}
		secret.Kind, secret.Title = "web", cmp.Or(server, account)
		secret.Protocol, secret.Port, secret.Auth = cmp.Or(protocolSchemes[protocol], protocol), attrs.number("port"), auth
	}
	return secret, true
}

// decryptKeychainItem unwraps an item's key with its protection class and
// decrypts the item to the DER of its attributes. It returns nil, nil for an
// item this cannot read as a matter of course — one the backup holds no key
// for (a non-migratory item, readable only on the device it came from) or in a
// version this does not read — and an error only for one that should have
// decrypted but did not.
func (c *Contents) decryptKeychainItem(blob []byte) ([]byte, error) {
	if c.keys == nil || len(blob) < 12 {
		return nil, nil
	}
	version := binary.LittleEndian.Uint32(blob)
	class := binary.LittleEndian.Uint32(blob[4:])
	wrappedLen := binary.LittleEndian.Uint32(blob[8:])
	if version != 3 || int(wrappedLen) > len(blob)-12 {
		return nil, nil
	}
	classKey, ok := c.keys[class]
	if !ok {
		return nil, nil
	}
	itemKey, err := aesUnwrap(classKey, blob[12:12+wrappedLen])
	if err != nil {
		return nil, fmt.Errorf("unwrap keychain item key: %w", err)
	}
	plain, err := gcmZeroIVDecrypt(itemKey, blob[12+wrappedLen:])
	if err != nil {
		return nil, fmt.Errorf("decrypt keychain item: %w", err)
	}
	return plain, nil
}

// gcmZeroIVDecrypt decrypts a keychain item, which iOS writes as AES-GCM with
// an all-zero J0. GCM's keystream is AES-CTR from J0+1, so the stdlib's CTR
// recovers the plaintext; the GCM tag is not checked because the stored object
// is already integrity-verified when it is read.
func gcmZeroIVDecrypt(key, data []byte) ([]byte, error) {
	const tagSize = 16
	if len(data) < tagSize {
		return nil, errors.New("keychain item is shorter than its tag")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	ciphertext := data[:len(data)-tagSize]
	counter := make([]byte, aes.BlockSize)
	counter[aes.BlockSize-1] = 1 // J0 is zero; the first keystream block is J0+1
	out := make([]byte, len(ciphertext))
	cipher.NewCTR(block, counter).XORKeyStream(out, ciphertext)
	return out, nil
}

// groupOwner is the longest of ids that group is or extends, "" for none: a
// group an app shares, such as "com.synology.DSfinder.SwitchAccount", is the
// app's.
func groupOwner[V any](group string, ids map[string]V) string {
	owner := ""
	for id := range ids {
		if (group == id || strings.HasPrefix(group, id+".")) && len(id) > len(owner) {
			owner = id
		}
	}
	return owner
}

// keychainAttrs are an item's attributes by name, as DER values.
type keychainAttrs map[string]asn1.RawValue

// text is a string or data attribute; "" when the item has none.
func (a keychainAttrs) text(name string) string { return string(a[name].Bytes) }

// code is a four-character code, such as a protocol, "htps"; "" for none,
// which an item may record as a zero.
func (a keychainAttrs) code(name string) string { return strings.Trim(a.text(name), " \x00") }

// number is an integer attribute; 0 when the item has none.
func (a keychainAttrs) number(name string) int {
	var n int
	if _, err := asn1.Unmarshal(a[name].FullBytes, &n); err != nil {
		return 0
	}
	return n
}

// parseKeychainAttributes reads an item's attributes from the DER iOS encodes
// them as: a set of (name, value) pairs.
func parseKeychainAttributes(der []byte) (keychainAttrs, error) {
	var pairs []struct {
		Name  string
		Value asn1.RawValue
	}
	if _, err := asn1.UnmarshalWithParams(der, &pairs, "set"); err != nil {
		return nil, err
	}
	attrs := make(keychainAttrs, len(pairs))
	for _, pair := range pairs {
		attrs[pair.Name] = pair.Value
	}
	return attrs, nil
}
