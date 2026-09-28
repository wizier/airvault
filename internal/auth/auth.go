package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	Username      = "airvault"
	tokenFileName = "auth-token"

	SessionCookie = "airvault_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type Credentials struct {
	token string
}

// The second result is the token to surface in the log; it is empty only when
// the token came from env.
func Load(configDir, configured string) (*Credentials, string, error) {
	if configured != "" {
		if err := validateToken(configured); err != nil {
			return nil, "", err
		}
		return &Credentials{token: configured}, "", nil
	}

	path := filepath.Join(configDir, tokenFileName)
	data, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if err := validateToken(token); err != nil {
			return nil, "", fmt.Errorf("reading auth token: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, "", fmt.Errorf("securing auth token: %w", err)
		}
		return &Credentials{token: token}, token, nil
	}
	if !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("reading auth token: %w", err)
	}

	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	token := base64.RawURLEncoding.EncodeToString(buf)
	if err := persistToken(configDir, path, token); err != nil {
		return nil, "", err
	}
	return &Credentials{token: token}, token, nil
}

func (c *Credentials) Valid(username, token string) bool {
	return secureEqual(username, Username) && secureEqual(token, c.token)
}

func validateToken(token string) error {
	if token == "" {
		return fmt.Errorf("AIRVAULT_AUTH_TOKEN must not be empty")
	}
	if strings.ContainsAny(token, "\r\n") {
		return fmt.Errorf("AIRVAULT_AUTH_TOKEN must be one line")
	}
	return nil
}

func persistToken(dir, path, token string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating auth directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".auth-token-*")
	if err != nil {
		return fmt.Errorf("creating auth token: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing auth token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing auth token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing auth token: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publishing auth token: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func secureEqual(left, right string) bool {
	a := sha256.Sum256([]byte(left))
	b := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (c *Credentials) IssueSession() (value string, expires time.Time) {
	return c.issueSessionAt(time.Now())
}

func (c *Credentials) issueSessionAt(now time.Time) (string, time.Time) {
	exp := now.Add(sessionTTL)
	payload := strconv.FormatInt(exp.Unix(), 10)
	return payload + "." + base64.RawURLEncoding.EncodeToString(c.sign(payload)), exp
}

func (c *Credentials) ValidSession(value string) bool {
	return c.validSessionAt(value, time.Now())
}

func (c *Credentials) validSessionAt(value string, now time.Time) bool {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	want := base64.RawURLEncoding.EncodeToString(c.sign(payload))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}
	return now.Before(time.Unix(exp, 0))
}

// sessionKey derives the cookie-signing secret from the token itself: no extra
// secret is persisted, and rotating the token invalidates every live session.
func (c *Credentials) sessionKey() []byte {
	sum := sha256.Sum256([]byte("airvault-session-v1\x00" + c.token))
	return sum[:]
}

func (c *Credentials) sign(payload string) []byte {
	mac := hmac.New(sha256.New, c.sessionKey())
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}
