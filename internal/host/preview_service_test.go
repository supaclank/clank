package host

import (
	"context"
	"errors"
	"testing"

	"github.com/supaclank/clank/internal/agent"
)

// TestPreviewWorkDirFor_CanceledContextSkipsDiskLookup pins the
// cancellation-first contract review-bot flagged: once the caller's
// context is done, previewWorkDirFor must report ctx.Err() rather than
// decoding the key and stat-ing disk for a folder-slug fallback that
// nobody will read the answer to.
func TestPreviewWorkDirFor_CanceledContextSkipsDiskLookup(t *testing.T) {
	t.Parallel()
	svc := New(Options{BackendManagers: map[agent.BackendType]agent.BackendManager{}})
	t.Cleanup(svc.Shutdown)

	slug := LocalRepoSlug(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := svc.previewWorkDirFor(ctx, slug)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("previewWorkDirFor() error = %v, want context.Canceled", err)
	}
}
