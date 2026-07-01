package github

// GET /credentials/github/repos/{owner}/{repo}/pulls — list the OPEN pull
// requests for a repo, for the repo-detail screen's "Pull requests" section.
// Mirrors ListBranches' paginate-and-trim approach; the token never leaves the
// host.

import (
	"context"
	"fmt"
	"time"

	gogithub "github.com/google/go-github/v66/github"
)

// maxPulls caps how many PRs ListPullRequests returns — the repo-detail UI
// shows a scannable list, not an unbounded feed.
const maxPulls = 100

// pullsPerPage is the page size for the PR listing pagination.
const pullsPerPage = 100

// PullRequestSummary is the trimmed PR shape returned to clients. HeadBranch is
// what the UI cross-references against loaded worktrees (to link a PR to a
// branch you already have) or forks from (to check a PR out).
type PullRequestSummary struct {
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	State      string    `json:"state"`
	Draft      bool      `json:"draft"`
	HTMLURL    string    `json:"html_url"`
	HeadBranch string    `json:"head_branch"`
	BaseBranch string    `json:"base_branch"`
	Author     string    `json:"author"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
}

// ListPullRequests lists the OPEN pull requests of owner/repo, most recently
// updated first, capped at maxPulls. token authenticates the call (private
// repos need it). Mirrors ListBranches' paginate-and-trim approach.
func (m *Manager) ListPullRequests(ctx context.Context, token, owner, repo string) ([]PullRequestSummary, error) {
	client, err := m.apiClient(token)
	if err != nil {
		return nil, fmt.Errorf("build api client: %w", err)
	}

	opts := &gogithub.PullRequestListOptions{
		State:       "open",
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gogithub.ListOptions{PerPage: pullsPerPage},
	}

	var out []PullRequestSummary
	for {
		prs, resp, err := client.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list pull requests: %w", err)
		}
		for _, pr := range prs {
			out = append(out, wirePRSummary(pr))
			if len(out) >= maxPulls {
				return out, nil
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// wirePRSummary collapses go-github's PullRequest to the trimmed summary.
func wirePRSummary(pr *gogithub.PullRequest) PullRequestSummary {
	return PullRequestSummary{
		Number:     pr.GetNumber(),
		Title:      pr.GetTitle(),
		State:      pr.GetState(),
		Draft:      pr.GetDraft(),
		HTMLURL:    pr.GetHTMLURL(),
		HeadBranch: pr.GetHead().GetRef(),
		BaseBranch: pr.GetBase().GetRef(),
		Author:     pr.GetUser().GetLogin(),
		UpdatedAt:  pr.GetUpdatedAt().Time,
	}
}
