package config

import "testing"

// An IPv6 bind host needs brackets, or the listener rejects the address; one
// already written with them (the only form that worked before) keeps working.
func TestListenAddrJoinsHostAndPort(t *testing.T) {
	t.Setenv("AIRVAULT_LISTEN_ADDR", "")
	t.Setenv("AIRVAULT_PORT", "8080")
	for host, want := range map[string]string{"0.0.0.0": "0.0.0.0:8080", "::": "[::]:8080", "[::]": "[::]:8080"} {
		t.Setenv("AIRVAULT_BIND_HOST", host)
		if got := Load().ListenAddr; got != want {
			t.Errorf("bind host %q: listen address %q, want %q", host, got, want)
		}
	}
}
