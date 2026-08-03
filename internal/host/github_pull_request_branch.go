package host

import (
	"context"
	"fmt"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/git"
)

func (s *Service) launchSameRepoPullRequest(
	ctx context.Context,
	req GitHubPullRequestLaunchRequest,
	inspection GitHubPullRequestInspection,
	token string,
	cloneURL string,
) (CreateWorktreeResult, error) {
	if !validBranchInput(inspection.HeadBranch) {
		return CreateWorktreeResult{}, fmt.Errorf("%w: invalid pull request branch %q", ErrInvalidArgument, inspection.HeadBranch)
	}
	repo, err := s.pullRequestRepo(ctx, req.Owner, req.Repo, true)
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
	result, err := s.pullRequestBranchWorktree(ctx, repo, inspection.HeadBranch, fetchedRef, req.Repo+fmt.Sprintf("#%d", req.Number))
	if err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(repo.gitDir, createdCanonical, err)
	}
	return result, nil
}

func (s *Service) pullRequestBranchWorktree(ctx context.Context, repo resolvedRepo, branch, fetchedRef, displayName string) (CreateWorktreeResult, error) {
	existing, err := git.FindWorktreeForBranch(repo.gitDir, branch)
	if err != nil {
		return CreateWorktreeResult{}, fmt.Errorf("find branch worktree: %w", err)
	}
	if existing != nil {
		worktreeID, err := agent.ReadLocalWorktreeID(existing.Path)
		if err != nil {
			return CreateWorktreeResult{}, fmt.Errorf("read worktree identity for %s: %w", existing.Path, err)
		}
		result := CreateWorktreeResult{
			WorktreeID:  worktreeID,
			Branch:      branch,
			WorktreeDir: existing.Path,
			DisplayName: displayName,
			OriginRepo:  repoDisplayLabel(repo.gitDir, repo.localCheckout),
			RepoSlug:    repo.slug,
		}
		return syncPullRequestWorktree(result, fetchedRef)
	}

	branchExists, err := git.BranchExists(repo.gitDir, branch)
	if err != nil {
		return CreateWorktreeResult{}, fmt.Errorf("check pull request branch: %w", err)
	}
	var worktree RepoWorktreeResult
	if branchExists {
		worktree, err = s.addRepoWorktree(ctx, repo.slug, repo.gitDir, repo.localCheckout, branch, displayName)
	} else {
		worktree, err = s.addRepoWorktreeNewBranch(ctx, repo.slug, repo.gitDir, repo.localCheckout, branch, fetchedRef)
		worktree.DisplayName = displayName
	}
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	return syncPullRequestWorktree(worktree.CreateWorktreeResult, fetchedRef)
}

func syncPullRequestWorktree(result CreateWorktreeResult, fetchedRef string) (CreateWorktreeResult, error) {
	dirty, err := git.WorkingTreeDirty(result.WorktreeDir)
	if err != nil {
		return CreateWorktreeResult{}, fmt.Errorf("check pull request worktree %s: %w", result.WorktreeDir, err)
	}
	if dirty {
		return CreateWorktreeResult{}, fmt.Errorf("%w: %s", ErrWorktreeDirty, result.WorktreeDir)
	}
	ahead, behind, err := git.AheadBehind(result.WorktreeDir, "HEAD", fetchedRef)
	if err != nil {
		return CreateWorktreeResult{}, fmt.Errorf("compare pull request branch at %s: %w", result.WorktreeDir, err)
	}
	switch {
	case ahead > 0 && behind > 0:
		return CreateWorktreeResult{}, fmt.Errorf("%w: %s", ErrRemoteDiverged, result.WorktreeDir)
	case ahead > 0:
		return CreateWorktreeResult{}, fmt.Errorf("%w: %s", ErrPullRequestLocalCommits, result.WorktreeDir)
	case behind > 0:
		if err := git.MergeFF(result.WorktreeDir, fetchedRef); err != nil {
			return CreateWorktreeResult{}, fmt.Errorf("fast-forward pull request branch at %s: %w", result.WorktreeDir, err)
		}
	}
	return result, nil
}
