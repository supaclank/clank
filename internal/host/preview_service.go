package host

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/preview"
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
// to a workdir and asks the preview manager to spawn the selected launch. An
// empty launch name resolves Expo or the configured default. Status carries the
// gateway-minted public URL + token when the host is wired to a
// gateway; on laptop dev with no gateway, those fields stay empty.
//
// Idempotent — a second call for the same key returns the existing
// snapshot.
func (s *Service) PreviewStart(ctx context.Context, worktreeID, launchName string) (preview.Status, error) {
	if s.preview == nil {
		return preview.Status{}, ErrPreviewUnavailable
	}
	workDir, err := s.previewWorkDirFor(ctx, worktreeID)
	if err != nil {
		return preview.Status{}, err
	}
	return s.preview.Start(ctx, worktreeID, workDir, launchName)
}

// PreviewStop terminates the selected service. An empty service name stops all
// services registered under the worktree, preserving the original mobile API.
func (s *Service) PreviewStop(_ context.Context, worktreeID, serviceName string) error {
	if s.preview == nil {
		return ErrPreviewUnavailable
	}
	if serviceName == "" {
		return s.preview.Stop(worktreeID)
	}
	return s.preview.StopService(worktreeID, serviceName)
}

// PreviewStatus returns availability and state for the selected launch on the
// preview key (worktree ID or folder slug).
func (s *Service) PreviewStatus(ctx context.Context, worktreeID, launchName string) (preview.Status, error) {
	if s.preview == nil {
		return preview.Status{}, ErrPreviewUnavailable
	}
	workDir, err := s.previewWorkDirFor(ctx, worktreeID)
	if err != nil {
		return preview.Status{}, err
	}
	return s.preview.StatusNamed(ctx, worktreeID, workDir, launchName)
}

// PreviewLogs returns the selected service's ANSI-stripped stdout/stderr tail.
// An empty name resolves Expo or the configured default, same as PreviewStatus.
// The result is bounded by the preview package's ring capacity and needs no
// pagination.
func (s *Service) PreviewLogs(ctx context.Context, worktreeID, serviceName string) []byte {
	if s.preview == nil {
		return nil
	}
	if serviceName != "" {
		return s.preview.LogTailNamed(worktreeID, serviceName)
	}
	workDir, err := s.previewWorkDirFor(ctx, worktreeID)
	if err != nil {
		return nil
	}
	return s.preview.LogTail(worktreeID, workDir)
}
