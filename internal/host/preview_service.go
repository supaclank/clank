package host

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/preview"
	"github.com/acksell/clank/pkg/preview/tokens"
)

// ErrPreviewUnavailable is returned by every PreviewXxx method when
// host.Service has no preview.Manager wired (today: only in tests that
// construct Service via host.New with a custom path). Exported as a
// sentinel so callers can errors.Is rather than string-match.
var ErrPreviewUnavailable = errors.New("preview: manager not configured on this host")

// previewWorkDirFor resolves a preview key to the directory the dev
// server runs in. The key namespace is two-tier: a managed worktree ID
// wins (the sprite/cloud path), else the key is decoded as the
// base64url slug of the previewed folder itself — the laptop
// `clank preview` path, where the folder may live anywhere on disk
// (a monorepo subdir, not necessarily a git repo). See LocalRepoSlug.
func (s *Service) previewWorkDirFor(ctx context.Context, key string) (string, error) {
	workDir, wtErr := s.workDirFor(ctx, agent.GitRef{WorktreeID: key})
	if wtErr == nil {
		return workDir, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	path, ok := localRepoPath(key)
	if !ok {
		return "", wtErr
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		// Moved or deleted folder — an honest 404, like a removed worktree.
		return "", fmt.Errorf("%w: preview folder %q", ErrNotFound, path)
	}
	return path, nil
}

// PreviewStart resolves the preview key (worktree ID or folder slug)
// to a workdir and asks the preview manager to spawn the dev server
// for the "default" service. The returned Status carries the
// gateway-minted public URL + token when the host is wired to a
// gateway; on laptop dev with no gateway, those fields stay empty.
//
// Idempotent — a second call for the same key returns the existing
// snapshot.
func (s *Service) PreviewStart(ctx context.Context, worktreeID string) (preview.Status, error) {
	if s.preview == nil {
		return preview.Status{}, ErrPreviewUnavailable
	}
	workDir, err := s.previewWorkDirFor(ctx, worktreeID)
	if err != nil {
		return preview.Status{}, err
	}
	return s.preview.Start(ctx, worktreeID, workDir, tokens.DefaultServiceName)
}

// PreviewStop terminates every dev server registered under worktreeID.
// In v1 that's the single "default" service. Returns ErrNotRunning
// when nothing's running — the mux maps it to 404.
func (s *Service) PreviewStop(_ context.Context, worktreeID string) error {
	if s.preview == nil {
		return ErrPreviewUnavailable
	}
	return s.preview.Stop(worktreeID)
}

// PreviewStatus returns availability + running state for the
// "default" service on the preview key (worktree ID or folder slug).
// Runs Detect every call so the Available bit reflects on-disk truth.
func (s *Service) PreviewStatus(ctx context.Context, worktreeID string) (preview.Status, error) {
	if s.preview == nil {
		return preview.Status{}, ErrPreviewUnavailable
	}
	workDir, err := s.previewWorkDirFor(ctx, worktreeID)
	if err != nil {
		return preview.Status{}, err
	}
	return s.preview.Status(ctx, worktreeID, workDir)
}

// PreviewLogs returns the most recent stdout/stderr captured from the
// "default" service's dev server (ANSI-stripped). Returns nil when no
// server is running. Bounded to ringCapacity in the preview package;
// safe to expose via HTTP without pagination.
func (s *Service) PreviewLogs(worktreeID string) []byte {
	if s.preview == nil {
		return nil
	}
	return s.preview.LogTail(worktreeID)
}
