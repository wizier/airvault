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

func TestSessionRejectsTamperedExpiredAndForeignValues(t *testing.T) {
	c := testCreds('a')
	value, _ := c.IssueSession()
	payload, sig, _ := strings.Cut(value, ".")
	expired, _ := c.issueSessionAt(time.Now().Add(-2 * sessionTTL))
	foreign, _ := testCreds('b').IssueSession()
	cases := map[string]string{
		"flipped signature": payload + "." + sig[:len(sig)-1] + "x",
		"forged expiry":     "99999999999." + sig,
		"missing separator": payload + sig,
		"empty":             "",
		"expired":           expired,
		"foreign secret":    foreign,
	}
	for name, rejected := range cases {
		if c.ValidSession(rejected) {
			t.Errorf("%s: session value should be rejected", name)
		}
	}
}
