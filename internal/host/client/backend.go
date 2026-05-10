package hostclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// BackendClient is a per-backend-type handle. Obtained via
// HTTP.Backend(bt).
type BackendClient struct {
	c  *HTTP
	bt agent.BackendType
}

// Backend returns a handle scoped to the given backend type. Use it for
// per-backend catalog and discovery operations.
func (c *HTTP) Backend(bt agent.BackendType) *BackendClient {
	return &BackendClient{c: c, bt: bt}
}

// Agents lists agents available for this backend in the given repo.
// The host resolves ref to a workdir internally — paths never cross the
// wire (§7.3). The three discrete GitRef fields are passed verbatim so
// the host can reconstruct the struct without canonical-form parsing.
func (b *BackendClient) Agents(ctx context.Context, ref agent.GitRef) ([]host.AgentInfo, error) {
	q := refQuery(b.bt, ref)
	var out []host.AgentInfo
	err := b.c.do(ctx, http.MethodGet, "/agents?"+q.Encode(), nil, &out)
	return out, err
}

// Models lists models available for this backend in the given repo.
// Same wire shape as Agents (see §7.3).
func (b *BackendClient) Models(ctx context.Context, ref agent.GitRef) ([]host.ModelInfo, error) {
	q := refQuery(b.bt, ref)
	var out []host.ModelInfo
	err := b.c.do(ctx, http.MethodGet, "/models?"+q.Encode(), nil, &out)
	return out, err
}

// refQuery serializes a GitRef as discrete query params. Mirrors
// hostmux.refFromQuery — keep the field names in sync.
func refQuery(bt agent.BackendType, ref agent.GitRef) url.Values {
	v := url.Values{"backend": {string(bt)}}
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

// Discover lists existing on-disk session snapshots for this backend
// rooted at seedDir.
func (b *BackendClient) Discover(ctx context.Context, seedDir string) ([]agent.SessionSnapshot, error) {
	body := struct {
		Backend agent.BackendType `json:"backend"`
		SeedDir string            `json:"seed_dir"`
	}{Backend: b.bt, SeedDir: seedDir}
	var out []agent.SessionSnapshot
	err := b.c.do(ctx, http.MethodPost, "/discover", body, &out)
	return out, err
}
