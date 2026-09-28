package iosbackup

import "testing"

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"18.5", "18.5", false},
		{"18.5", "18.6", false},
		{"18.6", "18.5", true},
		{"18.5.1", "18.5", true},
		{"18.5", "18.5.1", false},
		{"17.7", "18.0", false},
		{"26.0", "9.3", true},
		{"", "18.5", false},
		{"18.5", "", false},
	}
	for _, c := range cases {
		if got := NewerVersion(c.a, c.b); got != c.want {
			t.Errorf("NewerVersion(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
