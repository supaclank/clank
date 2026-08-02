package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/acksell/clank/internal/git"
	githubpkg "github.com/acksell/clank/internal/host/github"
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
	pr, err := s.github.GetPullRequest(ctx, "", locator.Owner, locator.Repo, locator.Number)
	if err != nil {
		if !errors.Is(err, githubpkg.ErrPullRequestNotFound) {
			return GitHubPullRequestInspection{}, "", err
		}
	} else {
		return inspectGitHubPullRequestResult(locator, pr, "")
	}

	token, err := s.github.AccessToken()
	if errors.Is(err, githubpkg.ErrNotConnected) {
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
	if !validGitObjectID(pr.HeadSHA) || pr.BaseBranch == "" {
		return GitHubPullRequestInspection{}, "", fmt.Errorf("GitHub returned incomplete pull request metadata")
	}
	return GitHubPullRequestInspection{
		GitHubPullRequestLocator: locator,
		Title:                    pr.Title,
		HTMLURL:                  pr.HTMLURL,
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

	slug, err := slugForImport(req.Owner, req.Repo)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	gitDir, err := s.canonicalGitDir(slug)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	defer s.lockRepo(slug)()

	cloneURL := fmt.Sprintf("%s/%s/%s.git", gitHubCloneBase, req.Owner, req.Repo)
	createdCanonical := false
	if _, statErr := os.Stat(gitDir); os.IsNotExist(statErr) {
		credentialHelper := ""
		if token != "" {
			credentialHelper = s.credentialHelperValue()
		}
		if err := s.cloneCanonical(ctx, cloneURL, gitDir, token, inspection.BaseBranch, req.Owner+"/"+req.Repo, credentialHelper); err != nil {
			return CreateWorktreeResult{}, err
		}
		createdCanonical = true
	} else if statErr != nil {
		return CreateWorktreeResult{}, fmt.Errorf("check canonical %q: %w", gitDir, statErr)
	} else if remoteURL, remoteErr := git.RemoteURL(gitDir, "origin"); remoteErr != nil || remoteURL != cloneURL {
		return CreateWorktreeResult{}, fmt.Errorf("canonical %q origin mismatch (have %q, want %q)", slug, remoteURL, cloneURL)
	}

	fetchedRef := pullRequestFetchRefPrefix + strconv.Itoa(req.Number) + "/" + expectedSHA
	refspec := fmt.Sprintf("+refs/pull/%d/head:%s", req.Number, fetchedRef)
	authHeader := ""
	if token != "" {
		authHeader = buildAuthHeader(token)
		if err := git.SetLocalConfig(gitDir, "credential.helper", s.credentialHelperValue()); err != nil {
			return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, err)
		}
	}
	if err := git.Fetch(gitDir, cloneURL, refspec, git.PushOptions{ExtraHeader: authHeader}); err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, fmt.Errorf("fetch pull request: %w", err))
	}
	fetchedSHA, err := git.RevParse(gitDir, fetchedRef)
	if err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, fmt.Errorf("resolve fetched pull request: %w", err))
	}
	if strings.ToLower(fetchedSHA) != expectedSHA {
		return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, fmt.Errorf("%w: approved %s, fetched %s", ErrPullRequestChanged, expectedSHA, fetchedSHA))
	}

	branch := pullRequestBranchPrefix + strconv.Itoa(req.Number) + "-" + expectedSHA[:pullRequestShortSHALength]
	branchExists, err := git.BranchExists(gitDir, branch)
	if err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, fmt.Errorf("check pull request branch: %w", err))
	}
	var result RepoWorktreeResult
	if branchExists {
		branchSHA, err := git.RevParse(gitDir, "refs/heads/"+branch)
		if err != nil {
			return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, err)
		}
		if strings.ToLower(branchSHA) != expectedSHA {
			return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, fmt.Errorf("pull request branch collision: %s resolves to %s", branch, branchSHA))
		}
		result, err = s.addRepoWorktree(ctx, slug, gitDir, false, branch, req.Repo+"#"+strconv.Itoa(req.Number))
	} else {
		result, err = s.addRepoWorktreeNewBranch(ctx, slug, gitDir, false, branch, fetchedRef)
		result.DisplayName = req.Repo + "#" + strconv.Itoa(req.Number)
	}
	if err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, err)
	}
	return result.CreateWorktreeResult, nil
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
