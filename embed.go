// Package airvault holds assets embedded into the binary: the goose migrations
// and the built web UI (Vite output under web/dist).
package airvault

import "embed"

//go:embed migrations/*.sql
var MigrationsFS embed.FS

//go:embed all:web/dist
var WebFS embed.FS
