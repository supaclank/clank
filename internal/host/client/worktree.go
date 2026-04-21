package hostclient

import (
	"context"
	"net/http"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// Worktree/branch operations are GitRef-scoped (the host repo registry
// was removed in §7.8). All requests carry the GitRef in the JSON body.

// ListBranches returns branches in the repo identified by ref.
func (c *HTTP) ListBranches(ctx context.Context, ref agent.GitRef) ([]host.BranchInfo, error) {
	body := struct {
		GitRef agent.GitRef `json:"git_ref"`
	}{ref}
	var out []host.BranchInfo
	err := c.do(ctx, http.MethodPost, "/worktrees/list-branches", body, &out)
	return out, err
}

// ResolveWorktree creates (or reuses) the worktree for branch and
// returns its info.
func (c *HTTP) ResolveWorktree(ctx context.Context, ref agent.GitRef, branch string) (host.WorktreeInfo, error) {
	body := struct {
		GitRef agent.GitRef `json:"git_ref"`
		Branch string       `json:"branch"`
	}{ref, branch}
	var out host.WorktreeInfo
	err := c.do(ctx, http.MethodPost, "/worktrees/resolve", body, &out)
	return out, err
}

// RemoveWorktree removes the worktree for branch. force forwards to git.
func (c *HTTP) RemoveWorktree(ctx context.Context, ref agent.GitRef, branch string, force bool) error {
	body := struct {
		GitRef agent.GitRef `json:"git_ref"`
		Branch string       `json:"branch"`
		Force  bool         `json:"force,omitempty"`
	}{ref, branch, force}
	return c.do(ctx, http.MethodPost, "/worktrees/remove", body, nil)
}

// MergeBranch merges branch into the repo's default branch.
func (c *HTTP) MergeBranch(ctx context.Context, ref agent.GitRef, branch, commitMessage string) (host.MergeResult, error) {
	body := struct {
		GitRef        agent.GitRef `json:"git_ref"`
		Branch        string       `json:"branch"`
		CommitMessage string       `json:"commit_message,omitempty"`
	}{ref, branch, commitMessage}
	var out host.MergeResult
	err := c.do(ctx, http.MethodPost, "/worktrees/merge", body, &out)
	return out, err
}
