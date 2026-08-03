package daemonclient

import (
	"context"
	"net/http"

	"github.com/supaclank/clank/internal/host"
)

// GitHubPullRequestLocator is the validated identity of a GitHub PR.
type GitHubPullRequestLocator struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// GitHubPullRequestInspection is the exact revision shown for approval.
type GitHubPullRequestInspection struct {
	GitHubPullRequestLocator
	Title      string `json:"title"`
	HTMLURL    string `json:"html_url"`
	HeadOwner  string `json:"head_owner"`
	HeadRepo   string `json:"head_repo"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	BaseBranch string `json:"base_branch"`
	Author     string `json:"author"`
	IsPrivate  bool   `json:"is_private"`
}

// GitHubPullRequestLaunchRequest binds launch to the revision the user approved.
type GitHubPullRequestLaunchRequest struct {
	GitHubPullRequestLocator
	ExpectedHeadSHA string `json:"expected_head_sha"`
}

// GitHubPullRequestInspect resolves the code identity without cloning it.
func (c *Client) GitHubPullRequestInspect(ctx context.Context, locator GitHubPullRequestLocator) (GitHubPullRequestInspection, error) {
	var out GitHubPullRequestInspection
	if err := c.githubDo(ctx, http.MethodPost, "/v1/github/pull-requests/inspect", locator, &out); err != nil {
		return GitHubPullRequestInspection{}, err
	}
	return out, nil
}

// GitHubPullRequestLaunch fetches and checks out the approved revision.
func (c *Client) GitHubPullRequestLaunch(ctx context.Context, req GitHubPullRequestLaunchRequest) (host.CreateWorktreeResult, error) {
	var out host.CreateWorktreeResult
	if err := c.githubDo(ctx, http.MethodPost, "/v1/github/pull-requests/launch", req, &out); err != nil {
		return host.CreateWorktreeResult{}, err
	}
	return out, nil
}
