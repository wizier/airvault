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

// The job id is added to context-aware records.
func WithJobID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, jobIDKey{}, id)
}

func Setup(level slog.Level) {
	info, err := os.Stdout.Stat()
	noColor := err != nil || info.Mode()&os.ModeCharDevice == 0
	slog.SetDefault(newRoot(os.Stdout, level, noColor))
}

func newRoot(out io.Writer, level slog.Level, noColor bool) *slog.Logger {
	handler := contextHandler{Handler: tint.NewTextHandler(out, &tint.Options{
		Level:      level,
		TimeFormat: timeFormat,
		NoColor:    noColor,
	})}
	return slog.New(handler).With("service", "airvault")
}

type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if id, _ := ctx.Value(jobIDKey{}).(string); id != "" {
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
