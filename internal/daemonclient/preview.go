package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrNotPreviewable mirrors the daemon's structured "no_preview" error:
// the folder has no detectable Expo or Vite app (host-side
// preview.ErrNotPreviewable). Callers gate user-facing "is this an
// Expo or Vite project?" hints on errors.Is — other preview-start
// failures (path resolution, spawn errors) are NOT this.
var ErrNotPreviewable = errors.New("preview: project is not previewable")

// codeNoPreview is the wire code hostmux writes for the host's
// preview.ErrNotPreviewable (see hostmux.writePreviewError).
const codeNoPreview = "no_preview"

// PreviewClient is the worktree-scoped handle for the Expo/Metro dev-server
// preview lifecycle. Routes proxy through the gateway to the owning host.
type PreviewClient struct {
	c          *Client
	worktreeID string
}

// Preview returns a handle bound to a worktree's dev-server preview.
func (c *Client) Preview(worktreeID string) *PreviewClient {
	return &PreviewClient{c: c, worktreeID: worktreeID}
}

// PreviewStatus is the dev-server state returned by Start/Status. Port is
// the dev server's listen port — populated even on the laptop path, where
// the gateway-minted public URL/Token fields stay empty. Kind mirrors
// preview.Kind ("expo" | "web") and tells `clank preview` which client
// flow to run (QR + phone vs browser overlay proxy).
type PreviewStatus struct {
	Available bool   `json:"available"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Port      int    `json:"port"`
	URL       string `json:"url"`
	Token     string `json:"token"`
}

// Start spawns (or returns the existing) dev server for the preview
// key — a managed worktree ID or a folder slug (host.LocalRepoSlug);
// the host resolves both. Idempotent on the host side.
func (p *PreviewClient) Start(ctx context.Context) (*PreviewStatus, error) {
	var s PreviewStatus
	if err := p.c.post(ctx, "/worktrees/"+p.worktreeID+"/preview/start", nil, &s); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == codeNoPreview {
			return nil, fmt.Errorf("%w: %s", ErrNotPreviewable, apiErr.Message)
		}
		return nil, err
	}
	return &s, nil
}

// Status returns availability + running state without spawning.
func (p *PreviewClient) Status(ctx context.Context) (*PreviewStatus, error) {
	var s PreviewStatus
	if err := p.c.get(ctx, "/worktrees/"+p.worktreeID+"/preview/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Stop terminates the worktree's dev server. Idempotent: a 404
// not_running is surfaced as an error by the transport, so callers that
// want naive idempotency should ignore it.
func (p *PreviewClient) Stop(ctx context.Context) error {
	return p.c.post(ctx, "/worktrees/"+p.worktreeID+"/preview/stop", nil, nil)
}

// Logs returns the dev server's captured stdout/stderr tail
// (text/plain, ANSI-stripped host-side; empty when nothing is
// running). Raw request — the shared do() helper assumes JSON bodies.
func (p *PreviewClient) Logs(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		p.c.baseURL+"/worktrees/"+p.worktreeID+"/preview/logs", nil)
	if err != nil {
		return nil, err
	}
	if p.c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.c.authToken)
	}
	resp, err := p.c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("daemon returned status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
