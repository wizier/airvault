package config

import (
	"cmp"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	ListenAddr string
	LogLevel   string

	BackupDir   string
	ConfigDir   string
	LockdownDir string
	MuxAddress  string

	DatabaseURL string

	AuthToken string // optional fixed HTTP Basic password; generated when empty
}

func Load() *Config {
	port := env("AIRVAULT_PORT", "8080")
	host := env("AIRVAULT_BIND_HOST", "127.0.0.1")
	configDir := env("AIRVAULT_CONFIG_DIR", "data/config")

	c := &Config{
		ListenAddr:  env("AIRVAULT_LISTEN_ADDR", net.JoinHostPort(strings.Trim(host, "[]"), port)),
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
	return cmp.Or(os.Getenv(key), def)
}
