// Package config loads AirVault's runtime configuration from the environment.
// Everything has a sane default so a bare run works; unRAID template fields and
// compose env_file both flow in through these same variables.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Config is the fully-resolved runtime configuration. Only genuinely GLOBAL
// settings live here.
type Config struct {
	ListenAddr string // host:port for the HTTP server
	LogLevel   string // debug | info | warn | error

	BackupDir   string
	ConfigDir   string
	LockdownDir string
	MuxAddress  string // loaded from USBMUXD_SOCKET_ADDRESS; immutable per engine

	DatabaseURL string

	AuthToken string // optional fixed HTTP Basic password; generated when empty
}

// Load reads the environment and applies defaults. Paths default to a local
// `data/` tree so a bare run works anywhere; containers override them via
// AIRVAULT_*_DIR env — one code path, no dev/prod fork.
func Load() *Config {
	port := env("AIRVAULT_PORT", "8080")
	host := env("AIRVAULT_BIND_HOST", "127.0.0.1")
	configDir := env("AIRVAULT_CONFIG_DIR", "data/config")

	c := &Config{
		ListenAddr:  env("AIRVAULT_LISTEN_ADDR", host+":"+port),
		LogLevel:    strings.ToLower(strings.TrimSpace(env("AIRVAULT_LOG_LEVEL", "info"))),
		BackupDir:   env("AIRVAULT_BACKUP_DIR", "data/backups"),
		ConfigDir:   configDir,
		LockdownDir: env("AIRVAULT_LOCKDOWN_DIR", "data/lockdown"),
		MuxAddress:  strings.TrimSpace(os.Getenv("USBMUXD_SOCKET_ADDRESS")),
		DatabaseURL: env("AIRVAULT_DATABASE_URL", filepath.Join(configDir, "airvault.db")),
		AuthToken:   strings.TrimSpace(env("AIRVAULT_AUTH_TOKEN", "")),
	}
	return c
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
