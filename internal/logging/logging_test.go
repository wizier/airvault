package logging

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func TestContextHandlerAddsJobID(t *testing.T) {
	var out bytes.Buffer
	logger := newRoot(&out, slog.LevelInfo, true).With("component", "go")
	logger.InfoContext(WithJobID(context.Background(), "job-123"), "started")

	got := out.String()
	for _, field := range []string{"INF", "started", "service=airvault", "component=go", "job_id=job-123"} {
		if !strings.Contains(got, field) {
			t.Fatalf("record does not contain %q: %s", field, got)
		}
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`).MatchString(got) {
		t.Fatalf("record does not start with a full timestamp: %s", got)
	}
}

func TestContextHandlerLeavesOrdinaryRecordAlone(t *testing.T) {
	var out bytes.Buffer
	logger := newRoot(&out, slog.LevelInfo, true)
	logger.Info("ready")

	if got := out.String(); strings.Contains(got, "job_id=") {
		t.Fatalf("ordinary record unexpectedly contains job id: %s", got)
	}
}
