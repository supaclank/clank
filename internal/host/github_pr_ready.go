package host

// Orchestration for "mark the worktree's PR ready for review".
// Mirrors github_pr.go's CreatePR: resolve worktree → read credential
// → find the branch's open PR → flip it via the GitHub API. No git
// mutations — purely a GitHub-side state change. The mux handler
// lives in internal/host/mux/github_pr.go.

import (
	"context"
	"errors"
	"fmt"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/git"
	githubpkg "github.com/supaclank/clank/internal/host/github"
)

// MarkPRReadyResult is the wire shape for POST /worktrees/{id}/pr/ready.
type MarkPRReadyResult struct {
	PRNumber int    `json:"pr_number"`
	PRURL    string `json:"pr_url"`
}

// ErrNoOpenPRForBranch fires when the worktree's branch has no open
// PR to mark ready. 404; clients refresh their remote status.
var ErrNoOpenPRForBranch = errors.New("no open pull request for this branch")

// MarkPRReady flips the open PR for the worktree's current branch
// from draft to ready-for-review. Idempotent: an already-ready PR
// succeeds without a mutation.
func (s *Service) MarkPRReady(ctx context.Context, ref agent.GitRef) (MarkPRReadyResult, error) {
	if s.github == nil {
		return MarkPRReadyResult{}, ErrGitHubManagerUnavailable
	}
	workdir, err := s.repoRootFor(ref)
	if err != nil {
		return MarkPRReadyResult{}, fmt.Errorf("resolve worktree: %w", err)
	}
	creds, err := s.github.Store().Read()
	if err != nil {
		return MarkPRReadyResult{}, fmt.Errorf("read github credentials: %w", err)
	}
	if creds.AccessToken == "" {
		return MarkPRReadyResult{}, ErrGitHubNotConnected
	}
	branch, err := git.CurrentBranch(workdir)
	if err != nil {
		return MarkPRReadyResult{}, fmt.Errorf("current branch: %w", err)
	}
	remoteURL, err := git.RemoteURL(workdir, "origin")
	if err != nil {
		return MarkPRReadyResult{}, ErrNoOriginRemote
	}
	owner, repo, err := githubpkg.ParseGitHubRemote(remoteURL)
	if err != nil {
		return MarkPRReadyResult{}, fmt.Errorf("parse origin url %q: %w", remoteURL, err)
	}
	pr, err := s.github.FindOpenPRForBranch(ctx, creds.AccessToken, owner, repo, branch)
	if err != nil {
		return MarkPRReadyResult{}, fmt.Errorf("find open pr: %w", err)
	}
	if pr == nil {
		return MarkPRReadyResult{}, ErrNoOpenPRForBranch
	}
	if err := s.github.MarkPRReadyForReview(ctx, creds.AccessToken, owner, repo, pr.Number); err != nil {
		return MarkPRReadyResult{}, err
	}
	return MarkPRReadyResult{PRNumber: pr.Number, PRURL: pr.HTMLURL}, nil
}
