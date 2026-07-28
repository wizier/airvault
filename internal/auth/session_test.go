package auth

import (
	"strings"
	"testing"
	"time"
)

func testCreds(seed byte) *Credentials {
	return &Credentials{token: strings.Repeat(string(seed), 24)}
}

func TestSessionRoundTrip(t *testing.T) {
	c := testCreds('a')
	value, exp := c.IssueSession()
	if !c.ValidSession(value) {
		t.Fatal("freshly issued session should be valid")
	}
	if time.Until(exp) <= 0 {
		t.Fatal("session should expire in the future")
	}
}

func TestSessionRejectsTampering(t *testing.T) {
	c := testCreds('a')
	value, _ := c.IssueSession()
	payload, sig, _ := strings.Cut(value, ".")
	cases := map[string]string{
		"flipped signature": payload + "." + sig[:len(sig)-1] + "x",
		"forged expiry":     "99999999999." + sig,
		"missing separator": payload + sig,
		"empty":             "",
	}
	for name, tampered := range cases {
		if c.ValidSession(tampered) {
			t.Errorf("%s: tampered value should be rejected", name)
		}
	}
}

func TestSessionRejectsExpired(t *testing.T) {
	c := testCreds('a')
	value, _ := c.issueSessionAt(time.Now().Add(-2 * sessionTTL))
	if c.validSessionAt(value, time.Now()) {
		t.Fatal("expired session should be rejected")
	}
}

func TestSessionRejectsForeignSecret(t *testing.T) {
	value, _ := testCreds('a').IssueSession()
	if testCreds('b').ValidSession(value) {
		t.Fatal("a session signed by a different token must not validate")
	}
}

func TestNilCredentialsRejectSession(t *testing.T) {
	var c *Credentials
	if c.ValidSession("anything") {
		t.Fatal("nil credentials must never authenticate")
	}
}
