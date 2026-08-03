package host

import (
	"context"
	"errors"
	"fmt"
	"strings"

	githubpkg "github.com/supaclank/clank/internal/host/github"
)

const (
	pullRequestFetchRefPrefix = "refs/clank/pull-requests/"
	pullRequestBranchPrefix   = "clank/pr-"
	pullRequestShortSHALength = 12
)

var (
	// ErrGitHubConnectionRequired means the PR may be private and no credential is available.
	ErrGitHubConnectionRequired = errors.New("connect GitHub to access this pull request")
	// ErrPullRequestChanged means the PR head no longer matches the approved SHA.
	ErrPullRequestChanged = errors.New("pull request changed after approval")
	// ErrPullRequestLocalCommits means the matching local branch contains commits outside the approved PR revision.
	ErrPullRequestLocalCommits = errors.New("pull request branch has local commits not present in the approved revision")
	// ErrPullRequestRepoAmbiguous means more than one host repository matches the PR repository.
	ErrPullRequestRepoAmbiguous = errors.New("more than one local checkout matches the pull request repository")
)

// GitHubPullRequestLocator identifies one PR without accepting an arbitrary clone URL.
type GitHubPullRequestLocator struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// GitHubPullRequestInspection is the exact revision a client asks a user to approve.
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

// GitHubPullRequestLaunchRequest binds a checkout to the SHA the user approved.
type GitHubPullRequestLaunchRequest struct {
	GitHubPullRequestLocator
	ExpectedHeadSHA string `json:"expected_head_sha"`
}

// InspectGitHubPullRequest resolves a PR without downloading or running its code.
func (s *Service) InspectGitHubPullRequest(ctx context.Context, locator GitHubPullRequestLocator) (GitHubPullRequestInspection, error) {
	inspection, _, err := s.inspectGitHubPullRequest(ctx, locator)
	return inspection, err
}

func (s *Service) inspectGitHubPullRequest(ctx context.Context, locator GitHubPullRequestLocator) (GitHubPullRequestInspection, string, error) {
	if !gitHubNamePattern.MatchString(locator.Owner) || !gitHubNamePattern.MatchString(locator.Repo) || locator.Number < 1 {
		return GitHubPullRequestInspection{}, "", fmt.Errorf("%w: invalid GitHub pull request locator", ErrInvalidArgument)
	}
	if s.github == nil {
		return GitHubPullRequestInspection{}, "", ErrGitHubManagerUnavailable
	}
	pr, anonymousErr := s.github.GetPullRequest(ctx, "", locator.Owner, locator.Repo, locator.Number)
	if anonymousErr != nil {
		if !errors.Is(anonymousErr, githubpkg.ErrPullRequestNotFound) && !errors.Is(anonymousErr, githubpkg.ErrPRForbidden) {
			return GitHubPullRequestInspection{}, "", anonymousErr
		}
	} else {
		return inspectGitHubPullRequestResult(locator, pr, "")
	}

	token, err := s.github.AccessToken()
	if errors.Is(err, githubpkg.ErrNotConnected) {
		if errors.Is(anonymousErr, githubpkg.ErrPRForbidden) {
			return GitHubPullRequestInspection{}, "", anonymousErr
		}
		return GitHubPullRequestInspection{}, "", ErrGitHubConnectionRequired
	}
	if err != nil {
		return GitHubPullRequestInspection{}, "", fmt.Errorf("github token: %w", err)
	}
	pr, err = s.github.GetPullRequest(ctx, token, locator.Owner, locator.Repo, locator.Number)
	if errors.Is(err, githubpkg.ErrPullRequestNotFound) {
		return GitHubPullRequestInspection{}, "", fmt.Errorf("%w: %s/%s#%d", ErrNotFound, locator.Owner, locator.Repo, locator.Number)
	}
	if err != nil {
		return GitHubPullRequestInspection{}, "", err
	}
	return inspectGitHubPullRequestResult(locator, pr, token)
}

func inspectGitHubPullRequestResult(locator GitHubPullRequestLocator, pr githubpkg.PullRequestDetails, token string) (GitHubPullRequestInspection, string, error) {
	if !validGitObjectID(pr.HeadSHA) || pr.HeadBranch == "" || pr.BaseBranch == "" {
		return GitHubPullRequestInspection{}, "", fmt.Errorf("GitHub returned incomplete pull request metadata")
	}
	return GitHubPullRequestInspection{
		GitHubPullRequestLocator: locator,
		Title:                    pr.Title,
		HTMLURL:                  pr.HTMLURL,
		HeadOwner:                pr.HeadOwner,
		HeadRepo:                 pr.HeadRepo,
		HeadBranch:               pr.HeadBranch,
		HeadSHA:                  strings.ToLower(pr.HeadSHA),
		BaseBranch:               pr.BaseBranch,
		Author:                   pr.Author,
		IsPrivate:                pr.IsPrivate,
	}, token, nil
}

// LaunchGitHubPullRequest fetches and checks out only the approved PR revision.
func (s *Service) LaunchGitHubPullRequest(ctx context.Context, req GitHubPullRequestLaunchRequest) (CreateWorktreeResult, error) {
	expectedSHA := strings.ToLower(req.ExpectedHeadSHA)
	if !validGitObjectID(expectedSHA) {
		return CreateWorktreeResult{}, fmt.Errorf("%w: expected_head_sha must be a full Git object id", ErrInvalidArgument)
	}
	inspection, token, err := s.inspectGitHubPullRequest(ctx, req.GitHubPullRequestLocator)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	if inspection.HeadSHA != expectedSHA {
		return CreateWorktreeResult{}, fmt.Errorf("%w: approved %s, current %s", ErrPullRequestChanged, expectedSHA, inspection.HeadSHA)
	}

	cloneURL := fmt.Sprintf("%s/%s/%s.git", gitHubCloneBase, req.Owner, req.Repo)
	if sameGitHubRepo(req.Owner, req.Repo, inspection.HeadOwner, inspection.HeadRepo) {
		return s.launchSameRepoPullRequest(ctx, req, inspection, token, cloneURL)
	}
	return s.launchForkPullRequest(ctx, req, inspection, token, cloneURL)
}

func validGitObjectID(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, r := range sha {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
