package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gogithub "github.com/google/go-github/v66/github"
)

// ErrPullRequestNotFound reports that GitHub did not expose the requested PR.
var ErrPullRequestNotFound = errors.New("github: pull request not found")

// PullRequestDetails identifies the exact code revision behind a GitHub PR.
type PullRequestDetails struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	HTMLURL    string `json:"html_url"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	BaseBranch string `json:"base_branch"`
	Author     string `json:"author"`
	IsPrivate  bool   `json:"is_private"`
}

// GetPullRequest returns one PR. An empty token deliberately makes an
// anonymous request, which is sufficient for public repositories.
func (m *Manager) GetPullRequest(ctx context.Context, token, owner, repo string, number int) (PullRequestDetails, error) {
	client, err := m.apiClient(token)
	if err != nil {
		return PullRequestDetails{}, fmt.Errorf("build api client: %w", err)
	}
	pr, resp, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusNotFound:
				return PullRequestDetails{}, fmt.Errorf("%w: %s/%s#%d", ErrPullRequestNotFound, owner, repo, number)
			case http.StatusUnauthorized:
				return PullRequestDetails{}, ErrPRTokenInvalid
			case http.StatusForbidden:
				return PullRequestDetails{}, ErrPRForbidden
			}
		}
		return PullRequestDetails{}, fmt.Errorf("get pull request: %w", err)
	}
	return wirePullRequest(pr), nil
}

func wirePullRequest(pr *gogithub.PullRequest) PullRequestDetails {
	return PullRequestDetails{
		Number:     pr.GetNumber(),
		Title:      pr.GetTitle(),
		HTMLURL:    pr.GetHTMLURL(),
		HeadBranch: pr.GetHead().GetRef(),
		HeadSHA:    pr.GetHead().GetSHA(),
		BaseBranch: pr.GetBase().GetRef(),
		Author:     pr.GetUser().GetLogin(),
		IsPrivate:  pr.GetBase().GetRepo().GetPrivate(),
	}
}
