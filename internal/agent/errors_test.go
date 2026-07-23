package agent

import (
	"context"
	"errors"
	"testing"
)

// Regression for the fork-500 class: an unsupported operation must be
// identifiable via errors.Is so the HTTP layer can map it to 501 instead
// of a generic internal error.
func TestClaudeFork_ReturnsErrUnsupported(t *testing.T) {
	t.Parallel()

	b := &ClaudeCodeBackend{}
	_, err := b.Fork(context.Background(), "msg-1")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Fork error = %v, want errors.Is(_, ErrUnsupported)", err)
	}
}
