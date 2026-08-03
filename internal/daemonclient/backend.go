package daemonclient

import (
	"context"
	"net/url"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/agent/presets"
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

// Presets returns the host's agent presets for this backend — built-ins
// (provisioner-declared) first, then user-saved ones. The Default preset's
// config keys double as the create-time required keys, so compose flows
// start from it.
func (b *BackendClient) Presets(ctx context.Context, hostname string) ([]presets.Preset, error) {
	v := url.Values{"backend": {string(b.backend)}, "hostname": {hostname}}
	var out []presets.Preset
	if err := b.c.get(ctx, "/presets?"+v.Encode(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ConfigOptions returns the agent's live advertised config options for
// this backend in ref's project, probed on demand by the host (one
// short-lived session). Slow by design — call it when a knob editor
// opens, behind a spinner, never on a hot path.
func (b *BackendClient) ConfigOptions(ctx context.Context, hostname string, ref agent.GitRef) ([]agent.ConfigOption, error) {
	path := "/config-options?" + catalogQuery(b.backend, hostname, ref).Encode()
	var out []agent.ConfigOption
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
	if ref.WorktreeID != "" {
		v.Set("git_worktree_id", ref.WorktreeID)
	}
	if ref.WorktreeBranch != "" {
		v.Set("worktree_branch", ref.WorktreeBranch)
	}
	return v
}
