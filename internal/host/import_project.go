package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

// gitHubNamePattern bounds an owner login or repository name to the
// characters GitHub allows. Enforced before the values are interpolated
// into a clone URL so a crafted "owner"/"repo" can't traverse paths or
// smuggle URL components.
var gitHubNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// gitHubCloneBase is the base the import clone URL is built from
// (<base>/<owner>/<repo>.git). Production is github.com; tests override it
// via SetGitHubCloneBaseForTest to clone a local repo without network.
var gitHubCloneBase = "https://github.com"

// SetGitHubCloneBaseForTest overrides gitHubCloneBase for the duration of
// a test, returning the previous value so the caller can restore it.
// Concurrent test access is unsafe — the override is a single global.
func SetGitHubCloneBaseForTest(base string) (prev string) {
	prev = gitHubCloneBase
	gitHubCloneBase = base
	return prev
}

// ImportProjectFromGitHub loads the caller's existing GitHub repo
// owner/repo under the repo-first layout: one bare BLOBLESS canonical
// clone at ~/work/repos/<slug>/repo.git (created on first import, reused
// after) plus a linked `git worktree` for the requested branch at
// ~/work/<WorktreeID>. The clone authenticates with the host's stored
// GitHub token, so private repos work; ErrNotConnected surfaces when no
// token is present.
//
// Idempotent per branch: importing a branch that already has a linked
// worktree returns that worktree (Created=false) instead of a duplicate —
// a branch can be checked out in at most one worktree (git's invariant,
// and the product's).
//
// The host builds the clone URL from owner/repo itself — it never accepts
// a client-supplied URL — matching the template flow's gatekeeping.
func (s *Service) ImportProjectFromGitHub(ctx context.Context, owner, repo, branch string) (CreateWorktreeResult, error) {
	if !gitHubNamePattern.MatchString(owner) {
		return CreateWorktreeResult{}, fmt.Errorf("%w: invalid owner %q", ErrInvalidArgument, owner)
	}
	if !gitHubNamePattern.MatchString(repo) {
		return CreateWorktreeResult{}, fmt.Errorf("%w: invalid repo %q", ErrInvalidArgument, repo)
	}
	// Branch is optional (empty → the remote's default). Branch names allow
	// "/", ".", "-" mid-name etc., so we don't pattern-match them, but a
	// leading "-" could be misread as a git flag — reject it.
	if strings.HasPrefix(branch, "-") {
		return CreateWorktreeResult{}, fmt.Errorf("%w: invalid branch %q", ErrInvalidArgument, branch)
	}

	gh := s.GitHub()
	if gh == nil {
		return CreateWorktreeResult{}, githubpkg.ErrNotConnected
	}
	token, err := gh.AccessToken()
	if err != nil {
		return CreateWorktreeResult{}, err
	}

	slug, err := slugForImport(owner, repo)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	gitDir, err := canonicalGitDir(slug)
	if err != nil {
		return CreateWorktreeResult{}, err
	}

	// All canonical mutations for this repo — first clone, branch
	// materialization, worktree add — serialize under the repo lock.
	defer s.lockRepo(slug)()

	cloneURL := fmt.Sprintf("%s/%s/%s.git", gitHubCloneBase, owner, repo)
	createdCanonical := false
	if _, statErr := os.Stat(gitDir); os.IsNotExist(statErr) {
		if err := s.cloneCanonical(ctx, cloneURL, gitDir, token, branch, owner+"/"+repo); err != nil {
			return CreateWorktreeResult{}, err
		}
		createdCanonical = true
	} else if statErr != nil {
		return CreateWorktreeResult{}, fmt.Errorf("check canonical %q: %w", gitDir, statErr)
	} else {
		// Canonical exists — make sure it's actually this repo and not a
		// slug collision against something else (paranoia: slugs are
		// deterministic, so this only fires on a corrupted layout).
		remoteURL, urlErr := git.RemoteURL(gitDir, "origin")
		if urlErr != nil || remoteURL != cloneURL {
			return CreateWorktreeResult{}, fmt.Errorf("canonical %q origin mismatch (have %q, want %q)", slug, remoteURL, cloneURL)
		}
	}

	// Resolve the branch to load: an explicit request, else the
	// canonical's HEAD (the remote's default at clone time).
	if branch == "" {
		branch, err = git.HeadBranch(gitDir)
		if err != nil {
			return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, fmt.Errorf("resolve default branch: %w", err))
		}
	}

	// The canonical is a --single-branch clone, so a branch other than
	// the one it was cloned with may have no local ref yet — fetch it
	// into the remote-tracking namespace before the worktree add. A
	// branch that's absent on the remote too falls through to
	// addRepoWorktree's ErrNotFound.
	if err := s.ensureRepoBranchAvailable(gitDir, branch); err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, err)
	}

	result, err := s.addRepoWorktree(ctx, slug, gitDir, branch, repo)
	if err != nil {
		return CreateWorktreeResult{}, s.rollbackCanonical(gitDir, createdCanonical, err)
	}
	if result.Created {
		s.log.Printf("imported %s branch %q → worktree %s", slug, branch, result.WorktreeID)
	}
	return result.CreateWorktreeResult, nil
}

// cloneCanonical creates the bare blobless canonical for a GitHub repo:
// clone, committer identity (worktree commits read config through the
// shared git dir), display label, and the persistent credential helper
// so lazy blob fetches + agent-run git can authenticate on their own.
func (s *Service) cloneCanonical(ctx context.Context, cloneURL, gitDir, token, branch, label string) error {
	if err := os.MkdirAll(filepath.Dir(gitDir), 0o755); err != nil {
		return fmt.Errorf("create canonical dir: %w", err)
	}
	if err := git.CloneBare(ctx, cloneURL, gitDir, token, branch, s.credentialHelperValue()); err != nil {
		// A failed clone can leave a partial gitDir behind — remove it so a
		// retry doesn't mistake it for an existing canonical.
		if rmErr := os.RemoveAll(filepath.Dir(gitDir)); rmErr != nil {
			s.log.Printf("warning: rollback partial canonical %s: %v", gitDir, rmErr)
		}
		return fmt.Errorf("clone canonical: %w", err)
	}
	if err := git.SetLocalConfig(gitDir, "user.name", s.projectCommitterName); err != nil {
		return fmt.Errorf("set config user.name: %w", err)
	}
	if err := git.SetLocalConfig(gitDir, "user.email", s.projectCommitterEmail); err != nil {
		return fmt.Errorf("set config user.email: %w", err)
	}
	if err := git.SetLocalConfig(gitDir, repoConfigLabelKey, label); err != nil {
		return fmt.Errorf("set config %s: %w", repoConfigLabelKey, err)
	}
	return nil
}

// RepoWorktreeResult pairs a CreateWorktreeResult with whether the call
// actually created the worktree (false = idempotent hit on an existing
// one). The wire shape of POST /repos/{slug}/worktrees.
type RepoWorktreeResult struct {
	CreateWorktreeResult
	Created bool `json:"created"`
}

// addRepoWorktree links a worktree for branch at ~/work/<newULID> off the
// canonical at gitDir, stamping the worktree id. When the branch is
// already checked out in a linked worktree, that worktree is returned
// instead (Created=false). branch must exist as refs/heads/<branch> or
// refs/remotes/origin/<branch> (the latter creates the local ref).
// displayName seeds CreateWorktreeResult.DisplayName.
//
// Caller holds the repo lock.
func (s *Service) addRepoWorktree(ctx context.Context, slug, gitDir, branch, displayName string) (RepoWorktreeResult, error) {
	// Idempotency: branch already loaded → hand back its worktree.
	if existing, err := git.FindWorktreeForBranch(gitDir, branch); err == nil && existing != nil {
		worktreeID, idErr := agent.ReadLocalWorktreeID(existing.Path)
		if idErr != nil || worktreeID == "" {
			return RepoWorktreeResult{}, fmt.Errorf("branch %q already checked out at %s but its worktree id is unreadable: %v", branch, existing.Path, idErr)
		}
		return RepoWorktreeResult{
			CreateWorktreeResult: CreateWorktreeResult{
				WorktreeID:  worktreeID,
				Branch:      branch,
				WorktreeDir: existing.Path,
				DisplayName: displayName,
				OriginRepo:  repoLabelFor(gitDir),
				RepoSlug:    slug,
			},
			Created: false,
		}, nil
	}

	root, err := workRootDir()
	if err != nil {
		return RepoWorktreeResult{}, err
	}
	worktreeULID, err := ulid.New(ulid.Now(), cryptoRand)
	if err != nil {
		return RepoWorktreeResult{}, fmt.Errorf("generate worktree id: %w", err)
	}
	worktreeID := worktreeULID.String()
	wtDir := filepath.Join(root, worktreeID)

	// Existing local branch checks out directly; a remote-tracking-only
	// branch gets its local ref created at the same tip.
	localExists, err := git.BranchExists(gitDir, branch)
	if err != nil {
		return RepoWorktreeResult{}, fmt.Errorf("check branch: %w", err)
	}
	if localExists {
		err = git.AddWorktree(gitDir, wtDir, branch)
	} else {
		remoteExists, remoteErr := git.RemoteTrackingBranchExists(gitDir, "origin", branch)
		if remoteErr != nil {
			return RepoWorktreeResult{}, fmt.Errorf("check remote branch: %w", remoteErr)
		}
		if !remoteExists {
			return RepoWorktreeResult{}, fmt.Errorf("%w: branch %q not found in %s", ErrNotFound, branch, slug)
		}
		err = git.AddWorktreeNewBranch(gitDir, wtDir, branch, "refs/remotes/origin/"+branch)
	}
	if err != nil {
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
		// Roll the half-made worktree back so a retry starts clean.
		if rmErr := git.RemoveWorktree(gitDir, wtDir, true); rmErr != nil {
			s.log.Printf("warning: rollback worktree %s: %v", wtDir, rmErr)
		}
		return RepoWorktreeResult{}, fmt.Errorf("stamp worktree-id: %w", err)
	}

	return RepoWorktreeResult{
		CreateWorktreeResult: CreateWorktreeResult{
			WorktreeID:  worktreeID,
			Branch:      branch,
			WorktreeDir: wtDir,
			DisplayName: displayName,
			OriginRepo:  repoLabelFor(gitDir),
			RepoSlug:    slug,
		},
		Created: true,
	}, nil
}

// rollbackCanonical removes a canonical this call created when a later
// step failed, so a retry doesn't find a half-initialized repo. Wraps
// and returns cause unchanged for `return` ergonomics.
func (s *Service) rollbackCanonical(gitDir string, createdByThisCall bool, cause error) error {
	if !createdByThisCall {
		return cause
	}
	if err := os.RemoveAll(filepath.Dir(gitDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Printf("warning: rollback canonical %s: %v", gitDir, err)
	}
	return cause
}
