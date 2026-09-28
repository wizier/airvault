package engine

import (
	"errors"
	"io/fs"
	"testing"
)

// A missing device item reads as fs.ErrNotExist; no other kind does.
func TestNotFoundIsErrNotExist(t *testing.T) {
	if !errors.Is(&Error{Kind: ErrorNotFound}, fs.ErrNotExist) {
		t.Fatal("ErrorNotFound must match fs.ErrNotExist")
	}
	if errors.Is(&Error{Kind: ErrorIntegrity}, fs.ErrNotExist) {
		t.Fatal("only ErrorNotFound matches fs.ErrNotExist")
	}
}
