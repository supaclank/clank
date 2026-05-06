package daemonclient

import (
	"context"
	"net/url"

	"github.com/acksell/clank/internal/agent"
)

// BackendClient is bound to a backend type. Backend selection on the wire
// is flat at the hub level — the hub picks the host internally.
type BackendClient struct {
	c       *Client
	backend agent.BackendType
}

// Backend returns a handle for the named backend.
func (c *Client) Backend(backend agent.BackendType) *BackendClient {
	return &BackendClient{c: c, backend: backend}
}

// Agents returns available agents for this backend, scoped to the
// (hostname, gitRef) tuple. Per §7.3, paths never cross the wire — the
// host resolves ref→workdir internally. The three discrete GitRef
// fields are sent verbatim so the hub mux can reconstruct the struct
// without canonical-form parsing.
func (b *BackendClient) Agents(ctx context.Context, hostname string, ref agent.GitRef) ([]agent.AgentInfo, error) {
	path := "/agents?" + catalogQuery(b.backend, hostname, ref).Encode()
	var out []agent.AgentInfo
	if err := b.c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Models returns available models for this backend, scoped to the
// (hostname, gitRef) tuple. Same wire shape as Agents (§7.3).
func (b *BackendClient) Models(ctx context.Context, hostname string, ref agent.GitRef) ([]agent.ModelInfo, error) {
	path := "/models?" + catalogQuery(b.backend, hostname, ref).Encode()
	var out []agent.ModelInfo
	if err := b.c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func catalogQuery(bt agent.BackendType, hostname string, ref agent.GitRef) url.Values {
	v := url.Values{
		"backend":  {string(bt)},
		"hostname": {string(hostname)},
	}
	if ref.LocalPath != "" {
		v.Set("git_local_path", ref.LocalPath)
	}
	if ref.RemoteURL != "" {
		v.Set("git_remote_url", ref.RemoteURL)
	}
	if ref.WorktreeBranch != "" {
		v.Set("worktree_branch", ref.WorktreeBranch)
	}
	return v
}
