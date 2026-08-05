package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/supaclank/clank/internal/git"
	githubpkg "github.com/supaclank/clank/internal/host/github"
)

var ErrGitHubRepositoryConnectionRequired = errors.New("connect GitHub to access this repository")

// GitHubRepositoryLocator identifies one repository without accepting an arbitrary clone URL.
type GitHubRepositoryLocator struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// GitHubRepositoryInspection is safe repository metadata shown before code is imported.
type GitHubRepositoryInspection struct {
	GitHubRepositoryLocator
	HTMLURL       string `json:"html_url"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	IsPrivate     bool   `json:"is_private"`
}

// GitHubRepositoryLaunchResult is the fresh editing worktree created from the
// repository's current remote default branch.
type GitHubRepositoryLaunchResult struct {
	CreateWorktreeResult
	DefaultBranch string `json:"default_branch"`
}

// InspectGitHubRepository resolves repository metadata without downloading code.
func (s *Service) InspectGitHubRepository(ctx context.Context, locator GitHubRepositoryLocator) (GitHubRepositoryInspection, error) {
	inspection, _, err := s.inspectGitHubRepository(ctx, locator)
	return inspection, err
}

func (s *Service) inspectGitHubRepository(ctx context.Context, locator GitHubRepositoryLocator) (GitHubRepositoryInspection, string, error) {
	if !gitHubNamePattern.MatchString(locator.Owner) || !gitHubNamePattern.MatchString(locator.Repo) {
		return GitHubRepositoryInspection{}, "", fmt.Errorf("%w: invalid GitHub repository locator", ErrInvalidArgument)
	}
	if s.github == nil {
		return GitHubRepositoryInspection{}, "", ErrGitHubManagerUnavailable
	}
	repository, anonymousErr := s.github.GetRepository(ctx, "", locator.Owner, locator.Repo)
	if anonymousErr == nil {
		return inspectGitHubRepositoryResult(locator, repository, "")
	}
	if !errors.Is(anonymousErr, githubpkg.ErrRepositoryNotFound) && !errors.Is(anonymousErr, githubpkg.ErrRepositoryForbidden) {
		return GitHubRepositoryInspection{}, "", anonymousErr
	}

	token, err := s.github.AccessToken()
	if errors.Is(err, githubpkg.ErrNotConnected) {
		// An anonymous API miss doesn't prove credentials are needed: once a
		// busy egress IP spends the tiny unauthenticated rate limit, GitHub
		// answers with 403 for public repositories too. Anonymous git
		// smart-HTTP carries no such limit, so let it decide whether the
		// repository is publicly clonable before demanding a connection.
		if inspection, isPublic := s.probePublicGitHubRepository(ctx, locator); isPublic {
			return inspection, "", nil
		}
		return GitHubRepositoryInspection{}, "", ErrGitHubRepositoryConnectionRequired
	}
	if err != nil {
		return GitHubRepositoryInspection{}, "", fmt.Errorf("github token: %w", err)
	}
	repository, err = s.github.GetRepository(ctx, token, locator.Owner, locator.Repo)
	if errors.Is(err, githubpkg.ErrRepositoryNotFound) {
		return GitHubRepositoryInspection{}, "", fmt.Errorf("%w: %s/%s", ErrNotFound, locator.Owner, locator.Repo)
	}
	if err != nil {
		return GitHubRepositoryInspection{}, "", err
	}
	return inspectGitHubRepositoryResult(locator, repository, token)
}

// probePublicGitHubRepository inspects a repository over anonymous git
// smart-HTTP after the REST API refused anonymous access. It yields no
// description — only the API serves that — but enough metadata to review
// and launch a public repository without any GitHub connection.
func (s *Service) probePublicGitHubRepository(ctx context.Context, locator GitHubRepositoryLocator) (GitHubRepositoryInspection, bool) {
	cloneURL := fmt.Sprintf("%s/%s/%s.git", gitHubCloneBase, locator.Owner, locator.Repo)
	defaultBranch, err := git.LsRemoteDefaultBranch(ctx, cloneURL)
	if err != nil {
		s.log.Printf("anonymous git probe of %s/%s failed: %v", locator.Owner, locator.Repo, err)
		return GitHubRepositoryInspection{}, false
	}
	s.log.Printf("inspected public repository %s/%s via anonymous git (API refused anonymous access)", locator.Owner, locator.Repo)
	return GitHubRepositoryInspection{
		GitHubRepositoryLocator: locator,
		HTMLURL:                 fmt.Sprintf("https://github.com/%s/%s", locator.Owner, locator.Repo),
		DefaultBranch:           defaultBranch,
		IsPrivate:               false,
	}, true
}

func inspectGitHubRepositoryResult(locator GitHubRepositoryLocator, repository githubpkg.RepositoryDetails, token string) (GitHubRepositoryInspection, string, error) {
	if repository.Owner == "" || repository.Name == "" || repository.HTMLURL == "" || repository.DefaultBranch == "" {
		return GitHubRepositoryInspection{}, "", errors.New("GitHub returned incomplete repository metadata")
	}
	return GitHubRepositoryInspection{
		GitHubRepositoryLocator: locator,
		HTMLURL:                 repository.HTMLURL,
		Description:             repository.Description,
		DefaultBranch:           repository.DefaultBranch,
		IsPrivate:               repository.IsPrivate,
	}, token, nil
}

// LaunchGitHubRepository imports the default branch, then forks a fresh editing
// branch from the latest remote default-branch tip without mutating that checkout.
func (s *Service) LaunchGitHubRepository(ctx context.Context, locator GitHubRepositoryLocator) (GitHubRepositoryLaunchResult, error) {
	inspection, token, err := s.inspectGitHubRepository(ctx, locator)
	if err != nil {
		return GitHubRepositoryLaunchResult{}, err
	}
	credentialHelper := ""
	if token != "" {
		credentialHelper = s.credentialHelperValue()
	}
	imported, err := s.importProjectFromGitHubWithAccess(ctx, locator.Owner, locator.Repo, inspection.DefaultBranch, token, credentialHelper)
	if err != nil {
		return GitHubRepositoryLaunchResult{}, err
	}

	repo, err := s.resolveRepoSlug(imported.RepoSlug)
	if err != nil {
		return GitHubRepositoryLaunchResult{}, err
	}
	defer s.lockRepo(repo.slug)()

	cloneURL := fmt.Sprintf("%s/%s/%s.git", gitHubCloneBase, locator.Owner, locator.Repo)
	refspec := "+refs/heads/" + inspection.DefaultBranch + ":refs/remotes/origin/" + inspection.DefaultBranch
	authHeader := ""
	if token != "" {
		authHeader = buildAuthHeader(token)
	}
	if err := git.Fetch(repo.gitDir, cloneURL, refspec, git.PushOptions{ExtraHeader: authHeader}); err != nil {
		return GitHubRepositoryLaunchResult{}, fmt.Errorf("fetch default branch %q: %w", inspection.DefaultBranch, err)
	}
	remoteRef := "refs/remotes/origin/" + inspection.DefaultBranch
	if _, err := git.RevParse(repo.gitDir, remoteRef); err != nil {
		return GitHubRepositoryLaunchResult{}, fmt.Errorf("resolve default branch %q: %w", inspection.DefaultBranch, err)
	}
	branch, err := availablePetnameBranch(repo.gitDir)
	if err != nil {
		return GitHubRepositoryLaunchResult{}, err
	}
	worktree, err := s.addRepoWorktreeNewBranch(ctx, repo.slug, repo.gitDir, repo.localCheckout, branch, remoteRef)
	if err != nil {
		return GitHubRepositoryLaunchResult{}, err
	}
	return GitHubRepositoryLaunchResult{
		CreateWorktreeResult: worktree.CreateWorktreeResult,
		DefaultBranch:        inspection.DefaultBranch,
	}, nil
}
