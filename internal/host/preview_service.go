package host

import (
	"context"
	"errors"
	"net/http"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/preview"
)

// ErrPreviewUnavailable is returned by every PreviewXxx method when
// host.Service has no preview.Manager wired (today: only in tests that
// construct Service via host.New with a custom path). Exported as a
// sentinel so callers can errors.Is rather than string-match.
var ErrPreviewUnavailable = errors.New("preview: manager not configured on this host")

// PreviewStart resolves worktreeID to a workdir and asks the preview
// manager to spawn the dev server. Idempotent — a second call for the
// same worktree returns the existing snapshot.
//
// previewURLBase is the full public URL Metro will bake into manifest
// URLs (e.g. "https://gateway/v1/worktrees/<wid>/preview/proxy"). The
// caller is the right authority for this: mobile knows its gateway,
// laptop curl tests pick localhost. clank-host cannot guess.
func (s *Service) PreviewStart(ctx context.Context, worktreeID, previewURLBase string) (preview.Status, error) {
	if s.preview == nil {
		return preview.Status{}, ErrPreviewUnavailable
	}
	workDir, err := s.workDirFor(ctx, agent.GitRef{WorktreeID: worktreeID})
	if err != nil {
		return preview.Status{}, err
	}
	return s.preview.Start(ctx, worktreeID, workDir, previewURLBase)
}

// PreviewStop terminates the dev server for worktreeID. ErrNotRunning
// when nothing's running — the mux maps it to 404.
func (s *Service) PreviewStop(_ context.Context, worktreeID string) error {
	if s.preview == nil {
		return ErrPreviewUnavailable
	}
	return s.preview.Stop(worktreeID)
}

// PreviewStatus returns availability + running state for worktreeID.
// Runs Detect every call so the Available bit reflects on-disk truth.
func (s *Service) PreviewStatus(ctx context.Context, worktreeID string) (preview.Status, error) {
	if s.preview == nil {
		return preview.Status{}, ErrPreviewUnavailable
	}
	workDir, err := s.workDirFor(ctx, agent.GitRef{WorktreeID: worktreeID})
	if err != nil {
		return preview.Status{}, err
	}
	return s.preview.Status(ctx, worktreeID, workDir)
}

// PreviewLogs returns the most recent stdout/stderr captured from the
// dev server (ANSI-stripped). Returns nil when no server is running.
// Bounded to ringCapacity in the preview package; safe to expose via
// HTTP without pagination.
func (s *Service) PreviewLogs(worktreeID string) []byte {
	if s.preview == nil {
		return nil
	}
	return s.preview.LogTail(worktreeID)
}

// PreviewProxyHandler returns the catch-all reverse proxy for
// worktreeID. prefixToStrip is the route prefix the dev server should
// not see — typically "/worktrees/<id>/preview/proxy".
//
// The returned handler 404s when no server is running for the
// worktree; callers do not need to pre-check via PreviewStatus.
func (s *Service) PreviewProxyHandler(worktreeID, prefixToStrip string) http.Handler {
	if s.preview == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, ErrPreviewUnavailable.Error(), http.StatusServiceUnavailable)
		})
	}
	return s.preview.ProxyHandler(worktreeID, prefixToStrip)
}
