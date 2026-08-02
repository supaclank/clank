package host

import (
	"context"
	"fmt"
	"strconv"

	"github.com/acksell/clank/internal/git"
)

func (s *Service) launchForkPullRequest(
	ctx context.Context,
	req GitHubPullRequestLaunchRequest,
	inspection GitHubPullRequestInspection,
	token string,
	cloneURL string,
) (CreateWorktreeResult, error) {
	repo, err := s.pullRequestRepo(ctx, req.Owner, req.Repo, false)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	defer s.lockRepo(repo.slug)()
	createdCanonical, err := s.ensurePullRequestRepo(ctx, repo, req.Owner, req.Repo, cloneURL, token, inspection.BaseBranch)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	fetchedRef, err := s.fetchApprovedPullRequest(repo, cloneURL, token, req.Number, inspection.HeadSHA)
	if err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(repo.gitDir, createdCanonical, err)
	}

	branch := pullRequestBranchPrefix + strconv.Itoa(req.Number) + "-" + inspection.HeadSHA[:pullRequestShortSHALength]
	branchExists, err := git.BranchExists(repo.gitDir, branch)
	if err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(repo.gitDir, createdCanonical, fmt.Errorf("check pull request branch: %w", err))
	}
	displayName := req.Repo + "#" + strconv.Itoa(req.Number)
	var result RepoWorktreeResult
	if branchExists {
		result, err = s.addRepoWorktree(ctx, repo.slug, repo.gitDir, false, branch, displayName)
	} else {
		result, err = s.addRepoWorktreeNewBranch(ctx, repo.slug, repo.gitDir, false, branch, fetchedRef)
		result.DisplayName = displayName
	}
	if err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(repo.gitDir, createdCanonical, err)
	}
	result.CreateWorktreeResult, err = syncPullRequestWorktree(result.CreateWorktreeResult, fetchedRef)
	if err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(repo.gitDir, createdCanonical, err)
	}
	return result.CreateWorktreeResult, nil
}
