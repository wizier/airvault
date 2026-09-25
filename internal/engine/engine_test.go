package engine

import (
	"context"
	"errors"
	"io"
	"testing"
)

// A slot closed under a live caller is a dead session, not a cancelled request.
func TestAFCErrorKeepsClosedSlotApartFromCancellation(t *testing.T) {
	closed := &Error{Kind: ErrorCancelled, Detail: "AFC session is closed"}
	if err := afcError(context.Background(), closed); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("live caller: %v, want io.ErrClosedPipe", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := afcError(ctx, closed); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: %v, want context.Canceled", err)
	}
}
