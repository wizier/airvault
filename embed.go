// Package airvault embeds the built web UI (Vite output under web/dist).
package airvault

import "embed"

//go:embed all:web/dist
var WebFS embed.FS
