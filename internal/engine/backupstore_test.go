package engine

import (
	"bytes"
	"testing"
)

func TestBackupKey(t *testing.T) {
	cases := map[string]string{
		"PHONE/Manifest.db":           "Manifest.db",
		"/PHONE//ab/./abc":            "ab/abc",
		"PHONE/../PHONE/Status.plist": "PHONE/Status.plist",
		"/.b/6/x":                     protocolDir + "/.b/6/x",
		"PHONE":                       "",
	}
	for devicePath, want := range cases {
		if key, err := backupKey("PHONE", devicePath); err != nil || key != want {
			t.Errorf("backupKey(%q) = %q, %v; want %q", devicePath, key, err, want)
		}
	}
	for _, devicePath := range []string{
		"", "/", "OTHER/Manifest.db", "../OTHER/x", "PHONE/" + protocolDir + "/x",
		"PHONE/a\\b", "PHONE/a\x00b", "PHONE/\u2028", "PHONE/\xff",
		"PHONE/" + string(bytes.Repeat([]byte("a"), 256)),
		"PHONE" + string(bytes.Repeat([]byte("/a"), 129)),
		"PHONE/" + string(bytes.Repeat([]byte("a/"), 2100)),
	} {
		if key, err := backupKey("PHONE", devicePath); err == nil {
			t.Errorf("backupKey(%.40q) = %q, want a refusal", devicePath, key)
		}
	}
}
