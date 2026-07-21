package host

// POST /repos/{slug}/worktrees — repo-scoped worktree creation, the
// replacement for POST /worktrees/create's "base worktree id" hack
// (which addressed a repo by whichever worktree the client happened to
// hold, and placed forks where sessions couldn't resolve them). Per
// CLAUDE.md's per-method-file rule this operation lives on its own.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host/petname"
)

// RepoWorktreeRequest is the body of POST /repos/{slug}/worktrees.
// Exactly one field must be set:
//
//   - Branch: LOAD an existing branch (local, or fetched from origin)
//     into a worktree. Idempotent — a branch that's already loaded
//     returns its existing worktree with created=false.
//   - BaseBranch: FORK a new petname branch off the named base and load
//     that.
type RepoWorktreeRequest struct {
	Branch     string `json:"branch,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
}

// CreateRepoWorktree creates (or idempotently returns) a worktree in the
// repo identified by slug. Branch resolution goes local ref →
// remote-tracking ref → one authed fetch from origin — which is what
// retires the "base branch doesn't exist in the shallow clone" 404
// class: the canonical can always materialize any branch its origin
// has. ErrRepoNotFound for an unknown slug; ErrNotFound for a branch
// that exists nowhere; ErrGitHubNotConnected when a needed fetch has no
// token.
func (s *Service) CreateRepoWorktree(ctx context.Context, slug string, req RepoWorktreeRequest) (RepoWorktreeResult, error) {
	if (req.Branch == "") == (req.BaseBranch == "") {
		return RepoWorktreeResult{}, fmt.Errorf("%w: exactly one of branch or base_branch is required", ErrInvalidArgument)
	}
	for _, b := range []string{req.Branch, req.BaseBranch} {
		if !validBranchInput(b) {
			return RepoWorktreeResult{}, fmt.Errorf("%w: invalid branch %q", ErrInvalidArgument, b)
		}
	}
	repo, err := s.resolveRepoSlug(slug)
	if err != nil {
		return RepoWorktreeResult{}, err
	}

	// A local checkout's default branch belongs to the user's primary
	// worktree — even when it happens not to be checked out right now,
	// loading it would block their next `git checkout <default>`.
	// Forking off it is always safe.
	if repo.localCheckout && req.Branch != "" {
		defaultBranch, err := git.DefaultBranch(repo.gitDir)
		if err != nil {
			return RepoWorktreeResult{}, fmt.Errorf("resolve default branch: %w", err)
		}
		if req.Branch == defaultBranch {
			return RepoWorktreeResult{}, fmt.Errorf("%w: %q", ErrReservedBranch, req.Branch)
		}
	}

	defer s.lockRepo(repo.slug)()

	if req.Branch != "" {
		return s.loadRepoBranch(ctx, repo.slug, repo.gitDir, repo.localCheckout, req.Branch)
	}
	return s.forkRepoBranch(ctx, repo.slug, repo.gitDir, repo.localCheckout, req.BaseBranch)
}

// loadRepoBranch checks the named branch out into a worktree (fetching
// it from origin when it isn't local yet). Caller holds the repo lock.
func (s *Service) loadRepoBranch(ctx context.Context, slug, gitDir string, localCheckout bool, branch string) (RepoWorktreeResult, error) {
	if err := s.ensureRepoBranchAvailable(gitDir, branch); err != nil {
		return RepoWorktreeResult{}, err
	}
	result, err := s.addRepoWorktree(ctx, slug, gitDir, localCheckout, branch, branch)
	if err != nil {
		return RepoWorktreeResult{}, err
	}
	if result.Created {
		s.log.Printf("loaded %s branch %q → worktree %s", slug, branch, result.WorktreeID)
	}
	return result, nil
}

// forkRepoBranch creates a fresh petname branch off base and loads it.
// The base may be a loaded branch's live ref, a canonical-local ref, or
// remote-only (fetched on demand). Caller holds the repo lock.
func (s *Service) forkRepoBranch(ctx context.Context, slug, gitDir string, localCheckout bool, base string) (RepoWorktreeResult, error) {
	if err := s.ensureRepoBranchAvailable(gitDir, base); err != nil {
		return RepoWorktreeResult{}, err
	}
	baseRef, err := resolveRepoRef(gitDir, base)
	if err != nil {
		return RepoWorktreeResult{}, fmt.Errorf("%w: base branch %q not found in %s", ErrNotFound, base, slug)
	}

	branch, err := availablePetnameBranch(gitDir)
	if err != nil {
		return RepoWorktreeResult{}, err
	}
	result, err := s.addRepoWorktreeNewBranch(ctx, slug, gitDir, localCheckout, branch, baseRef)
	if err != nil {
		return RepoWorktreeResult{}, err
	}
	s.log.Printf("forked %s branch %q off %q → worktree %s", slug, branch, base, result.WorktreeID)
	return result, nil
}

// resolveRepoRef returns the ref to base a fork on: the local branch
// when present (a loaded branch's LIVE tip — its worktree's commits
// count), else the remote-tracking ref. Errors when neither resolves.
func resolveRepoRef(gitDir, branch string) (string, error) {
	local, err := git.BranchExists(gitDir, branch)
	if err != nil {
		return "", err
	}
	if local {
		return "refs/heads/" + branch, nil
	}
	tracking, err := git.RemoteTrackingBranchExists(gitDir, "origin", branch)
	if err != nil {
		return "", err
	}
	if tracking {
		return "refs/remotes/origin/" + branch, nil
	}
	return "", fmt.Errorf("branch %q not found", branch)
}

// addRepoWorktreeNewBranch is addRepoWorktree's fork twin: create a NEW
// branch at baseRef and link its worktree at ~/work/<newULID>. No
// idempotency arm — the petname is freshly minted under the repo lock.
func (s *Service) addRepoWorktreeNewBranch(ctx context.Context, slug, gitDir string, localCheckout bool, branch, baseRef string) (RepoWorktreeResult, error) {
	root, err := s.workRootDir()
	if err != nil {
		return RepoWorktreeResult{}, err
	}
	worktreeULID, err := ulid.New(ulid.Now(), cryptoRand)
	if err != nil {
		return RepoWorktreeResult{}, fmt.Errorf("generate worktree id: %w", err)
	}
	worktreeID := worktreeULID.String()
	wtDir := filepath.Join(root, worktreeID)

	if err := git.AddWorktreeNewBranch(gitDir, wtDir, branch, baseRef); err != nil {
		// A failed `git worktree add` can still leave a partial wtDir and/or
		// worktree bookkeeping behind — best-effort clean so a retry starts fresh.
		if rmErr := os.RemoveAll(wtDir); rmErr != nil {
			s.log.Printf("warning: cleanup partial worktree dir %s: %v", wtDir, rmErr)
		}
		if pruneErr := git.PruneWorktrees(gitDir); pruneErr != nil {
			s.log.Printf("warning: prune worktrees in %s: %v", gitDir, pruneErr)
		}
		return RepoWorktreeResult{}, fmt.Errorf("add worktree: %w", err)
	}
	if err := agent.WriteLocalWorktreeID(wtDir, worktreeID); err != nil {
		// Best-effort rollback so a retry doesn't accumulate orphaned
		// worktrees + branches. Logged-and-continue: the stamp error is
		// what the caller needs to see.
		if rmErr := git.RemoveWorktree(gitDir, wtDir, true); rmErr != nil {
			s.log.Printf("warning: rollback worktree %s: %v", wtDir, rmErr)
		}
		if delErr := git.DeleteBranch(gitDir, branch, true); delErr != nil {
			s.log.Printf("warning: rollback delete branch %q: %v", branch, delErr)
		}
		return RepoWorktreeResult{}, fmt.Errorf("stamp worktree-id: %w", err)
	}
	return RepoWorktreeResult{
		CreateWorktreeResult: CreateWorktreeResult{
			WorktreeID:  worktreeID,
			Branch:      branch,
			WorktreeDir: wtDir,
			DisplayName: branch,
			OriginRepo:  repoDisplayLabel(gitDir, localCheckout),
			RepoSlug:    slug,
		},
		Created: true,
	}, nil
}

// validBranchInput reports whether b is safe to hand to git as a branch
// name: guards against flag injection ('-' prefix) and whitespace/control
// characters, which git's ref grammar rejects deep inside `worktree add`
// with a bare "fatal: Refusing to set 'HEAD' to invalid ref" — surfacing
// as a 500 instead of a clean 400 invalid_argument. Emptiness is checked
// separately by the caller.
func validBranchInput(b string) bool {
	if b == "" {
		return true
	}
	if b[0] == '-' {
		return false
	}
	for _, r := range b {
		if r <= ' ' {
			return false
		}
	}
	return true
}

// availablePetnameBranch generates a petname branch name that doesn't
// collide with an existing local branch. Caller holds the repo lock, so
// the name stays free until the worktree add claims it.
func availablePetnameBranch(gitDir string) (string, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		candidate := petname.Generate()
		exists, err := git.BranchExists(gitDir, candidate)
		if err != nil {
			return "", fmt.Errorf("check candidate branch: %w", err)
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not generate a unique petname after %d attempts", maxAttempts)
}
