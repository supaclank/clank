package daemonclient

import (
	"context"
	"net/url"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// HostClient is the hub-side handle for one host. Bound to a hostname.
//
// Per §7 of the hub-host refactor the host plane no longer keeps a repo
// registry; identity on the wire is `(host, GitRef, branch)` and ref is
// always sent in the request body. The methods on HostClient are flat —
// there is no Repo/Worktree builder chain because the host itself has
// no per-repo handle to bind to.
type HostClient struct {
	c        *Client
	hostname host.Hostname
}

// Host returns a handle for the named host.
func (c *Client) Host(hostname host.Hostname) *HostClient {
	return &HostClient{c: c, hostname: hostname}
}

// Hostname returns the hostname this handle is bound to.
func (h *HostClient) Hostname() host.Hostname { return h.hostname }

func (h *HostClient) base() string {
	return "/hosts/" + url.PathEscape(string(h.hostname))
}

// listBranchesRequest is the JSON body for POST /hosts/{h}/worktrees/list-branches.
type listBranchesRequest struct {
	GitRef agent.GitRef `json:"git_ref"`
}

// ListBranches returns the branches/worktrees for the given repo. The
// branch on ref is ignored — the response enumerates all branches.
func (h *HostClient) ListBranches(ctx context.Context, ref agent.GitRef) ([]host.BranchInfo, error) {
	var out []host.BranchInfo
	if err := h.c.post(ctx, h.base()+"/worktrees/list-branches", listBranchesRequest{GitRef: ref}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type worktreeMutationRequest struct {
	GitRef agent.GitRef `json:"git_ref"`
	Branch string       `json:"branch"`
}

// ResolveWorktree creates (or reuses) the worktree for branch and
// returns its info.
func (h *HostClient) ResolveWorktree(ctx context.Context, ref agent.GitRef, branch string) (host.WorktreeInfo, error) {
	var out host.WorktreeInfo
	if err := h.c.post(ctx, h.base()+"/worktrees/resolve", worktreeMutationRequest{GitRef: ref, Branch: branch}, &out); err != nil {
		return host.WorktreeInfo{}, err
	}
	return out, nil
}

type removeWorktreeRequest struct {
	GitRef agent.GitRef `json:"git_ref"`
	Branch string       `json:"branch"`
	Force  bool         `json:"force,omitempty"`
}

// RemoveWorktree deletes the worktree for branch. force forwards to git.
func (h *HostClient) RemoveWorktree(ctx context.Context, ref agent.GitRef, branch string, force bool) error {
	return h.c.post(ctx, h.base()+"/worktrees/remove", removeWorktreeRequest{GitRef: ref, Branch: branch, Force: force}, nil)
}

type mergeBranchRequest struct {
	GitRef        agent.GitRef `json:"git_ref"`
	Branch        string       `json:"branch"`
	CommitMessage string       `json:"commit_message,omitempty"`
}

// MergeBranch merges branch into the repo's default branch using
// commitMessage.
func (h *HostClient) MergeBranch(ctx context.Context, ref agent.GitRef, branch, commitMessage string) (host.MergeResult, error) {
	var out host.MergeResult
	if err := h.c.post(ctx, h.base()+"/worktrees/merge", mergeBranchRequest{GitRef: ref, Branch: branch, CommitMessage: commitMessage}, &out); err != nil {
		return host.MergeResult{}, err
	}
	return out, nil
}
