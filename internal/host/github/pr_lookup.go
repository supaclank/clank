package github

// PR discovery for a branch — powers the "view PR" link the host attaches
// to a worktree's remote status, without persisting any PR state.

import (
	"context"
	"fmt"

	gogithub "github.com/google/go-github/v66/github"
)

// FindOpenPRForBranch returns the open PR whose head is owner:branch, or
// nil when none exists. (nil, nil) means "no open PR"; a non-nil error is
// a real API/transport failure the caller can treat as best-effort.
func (m *Manager) FindOpenPRForBranch(ctx context.Context, accessToken, owner, repo, branch string) (*PullRequest, error) {
	client, err := m.apiClient(accessToken)
	if err != nil {
		return nil, fmt.Errorf("build api client: %w", err)
	}
	return firstOpenPR(ctx, client, owner, repo, branch)
}

// firstOpenPR returns the first open PR for owner:head, or nil when there
// is none. Shared by FindOpenPRForBranch and the create-PR "already
// exists" follow-up so both speak to GitHub the same way.
func firstOpenPR(ctx context.Context, client *gogithub.Client, owner, repo, head string) (*PullRequest, error) {
	prs, _, err := client.PullRequests.List(ctx, owner, repo, &gogithub.PullRequestListOptions{
		Head:        owner + ":" + head,
		State:       "open",
		ListOptions: gogithub.ListOptions{PerPage: 1},
	})
	if err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	pr := wirePR(prs[0])
	return &pr, nil
}
