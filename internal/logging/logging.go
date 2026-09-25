// Package logging owns AirVault's process-wide text logger and the small
// operation context shared by the Go service and the embedded Rust engine.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"

	"github.com/lmittmann/tint"
)

const timeFormat = "2006-01-02 15:04:05"

type jobIDKey struct{}

// WithJobID attaches a runtime correlation id to ctx. The handler below adds it
// to context-aware records; the cgo bridge also reads it before entering Rust.
func WithJobID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, jobIDKey{}, id)
}

// JobID returns the runtime correlation id carried by ctx, if any.
func JobID(ctx context.Context) string {
	id, _ := ctx.Value(jobIDKey{}).(string)
	return id
}

var root = slog.Default()

// Setup installs the one text renderer used by Go and bridged Rust events.
func Setup(level slog.Level) {
	info, err := os.Stdout.Stat()
	noColor := err != nil || info.Mode()&os.ModeCharDevice == 0
	root = newRoot(os.Stdout, level, noColor)
	slog.SetDefault(root.With("component", "go"))
}

func newRoot(out io.Writer, level slog.Level, noColor bool) *slog.Logger {
	handler := contextHandler{Handler: tint.NewTextHandler(out, &tint.Options{
		Level:      level,
		TimeFormat: timeFormat,
		NoColor:    noColor,
	})}
	return slog.New(handler).With("service", "airvault")
}

// Component returns a logger sharing the process renderer and carrying a
// stable source label. Rust's tracing bridge uses component=engine.rust.
func Component(name string) *slog.Logger {
	return root.With("component", name)
}

// contextHandler enriches only records that were intentionally logged with a
// context; ordinary background logs do not grow synthetic identifiers.
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if id := JobID(ctx); id != "" {
		record = record.Clone()
		record.AddAttrs(slog.String("job_id", id))
	}
	return h.Handler.Handle(ctx, record)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name)}
}
