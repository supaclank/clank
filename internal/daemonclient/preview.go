package daemonclient

import "context"

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

// Start spawns (or returns the existing) dev server for the worktree.
// Idempotent on the host side. A non-empty localPath starts an in-place
// preview on that folder (laptop `clank preview`) instead of resolving a
// ~/work worktree by id.
func (p *PreviewClient) Start(ctx context.Context, localPath string) (*PreviewStatus, error) {
	var body any
	if localPath != "" {
		body = map[string]string{"local_path": localPath}
	}
	var s PreviewStatus
	if err := p.c.post(ctx, "/worktrees/"+p.worktreeID+"/preview/start", body, &s); err != nil {
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
