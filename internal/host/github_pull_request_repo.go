package host

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/acksell/clank/internal/git"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

func sameGitHubRepo(ownerA, repoA, ownerB, repoB string) bool {
	return strings.EqualFold(ownerA, ownerB) && strings.EqualFold(repoA, repoB)
}

// pullRequestRepo finds the host-side repository for a PR. A user's own
// checkout wins over a managed canonical so its branch and sessions survive.
func (s *Service) pullRequestRepo(ctx context.Context, owner, name string, preferLocal bool) (resolvedRepo, error) {
	if preferLocal {
		locals, err := s.discoveredLocalRepos(ctx)
		if err != nil {
			return resolvedRepo{}, fmt.Errorf("discover local repositories: %w", err)
		}
		locals = matchingPullRequestRepos(locals, owner, name)
		switch len(locals) {
		case 1:
			return s.resolveRepoSlug(locals[0].Slug)
		case 0:
		default:
			paths := make([]string, 0, len(locals))
			for _, repo := range locals {
				paths = append(paths, repo.Path)
			}
			return resolvedRepo{}, fmt.Errorf("%w: %s", ErrPullRequestRepoAmbiguous, strings.Join(paths, ", "))
		}
	}
	repos, err := s.ListRepos(ctx)
	if err != nil {
		return resolvedRepo{}, fmt.Errorf("list repositories: %w", err)
	}
	canonicals := make([]RepoInfo, 0, len(repos))
	for _, repo := range matchingPullRequestRepos(repos, owner, name) {
		if !repo.IsLocalCheckout {
			canonicals = append(canonicals, repo)
		}
	}
	switch len(canonicals) {
	case 1:
		return s.resolveRepoSlug(canonicals[0].Slug)
	case 0:
	default:
		return resolvedRepo{}, fmt.Errorf("%w: managed repositories %s", ErrPullRequestRepoAmbiguous, owner+"/"+name)
	}

	slug, err := slugForImport(owner, name)
	if err != nil {
		return resolvedRepo{}, err
	}
	gitDir, err := s.canonicalGitDir(slug)
	if err != nil {
		return resolvedRepo{}, err
	}
	return resolvedRepo{gitDir: gitDir, slug: slug}, nil
}

func matchingPullRequestRepos(repos []RepoInfo, owner, name string) []RepoInfo {
	matches := make([]RepoInfo, 0, len(repos))
	for _, repo := range repos {
		if repo.Origin != nil && sameGitHubRepo(owner, name, repo.Origin.Owner, repo.Origin.Repo) {
			matches = append(matches, repo)
		}
	}
	return matches
}

// ensurePullRequestRepo clones a missing canonical and validates an existing
// one. Local checkouts were already validated by resolveRepoSlug.
func (s *Service) ensurePullRequestRepo(ctx context.Context, repo resolvedRepo, owner, name, cloneURL, token, baseBranch string) (bool, error) {
	if repo.localCheckout {
		return false, nil
	}
	if _, err := os.Stat(repo.gitDir); os.IsNotExist(err) {
		credentialHelper := ""
		if token != "" {
			credentialHelper = s.credentialHelperValue()
		}
		if err := s.cloneCanonical(ctx, cloneURL, repo.gitDir, token, baseBranch, owner+"/"+name, credentialHelper); err != nil {
			return false, err
		}
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("check canonical %q: %w", repo.slug, err)
	}
	remoteURL, err := git.RemoteURL(repo.gitDir, "origin")
	if err != nil {
		return false, fmt.Errorf("get canonical %q origin: %w", repo.slug, err)
	}
	remoteOwner, remoteRepo, parseErr := githubpkg.ParseGitHubRemote(remoteURL)
	if remoteURL != cloneURL && (parseErr != nil || !sameGitHubRepo(owner, name, remoteOwner, remoteRepo)) {
		return false, fmt.Errorf("canonical %q origin mismatch (have %q, want %s/%s)", repo.slug, remoteURL, owner, name)
	}
	return false, nil
}

func (s *Service) fetchApprovedPullRequest(repo resolvedRepo, cloneURL, token string, number int, expectedSHA string) (string, error) {
	fetchedRef := pullRequestFetchRefPrefix + fmt.Sprintf("%d/%s", number, expectedSHA)
	refspec := fmt.Sprintf("+refs/pull/%d/head:%s", number, fetchedRef)
	authHeader := ""
	if token != "" {
		authHeader = buildAuthHeader(token)
		if !repo.localCheckout {
			if err := git.SetLocalConfig(repo.gitDir, "credential.helper", s.credentialHelperValue()); err != nil {
				return "", err
			}
		}
	}
	if err := git.Fetch(repo.gitDir, cloneURL, refspec, git.PushOptions{ExtraHeader: authHeader}); err != nil {
		return "", fmt.Errorf("fetch pull request: %w", err)
	}
	fetchedSHA, err := git.RevParse(repo.gitDir, fetchedRef)
	if err != nil {
		return "", fmt.Errorf("resolve fetched pull request: %w", err)
	}
	if strings.ToLower(fetchedSHA) != expectedSHA {
		return "", fmt.Errorf("%w: approved %s, fetched %s", ErrPullRequestChanged, expectedSHA, fetchedSHA)
	}
	return fetchedRef, nil
}
