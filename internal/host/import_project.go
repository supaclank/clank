package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

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

// ImportProjectFromGitHub clones the caller's existing GitHub repo
// owner/repo into a fresh ~/work/<WorktreeID> project, keeping the .git
// directory and the origin remote (unlike CreateProjectFromTemplate,
// which discards them). The clone authenticates with the host's stored
// GitHub token, so private repos work; ErrNotConnected surfaces when no
// token is present.
//
// The host builds the clone URL from owner/repo itself — it never accepts
// a client-supplied URL — matching the template flow's gatekeeping.
func (s *Service) ImportProjectFromGitHub(ctx context.Context, owner, repo string) (CreateWorktreeResult, error) {
	if !gitHubNamePattern.MatchString(owner) {
		return CreateWorktreeResult{}, fmt.Errorf("%w: invalid owner %q", ErrInvalidArgument, owner)
	}
	if !gitHubNamePattern.MatchString(repo) {
		return CreateWorktreeResult{}, fmt.Errorf("%w: invalid repo %q", ErrInvalidArgument, repo)
	}

	gh := s.GitHub()
	if gh == nil {
		return CreateWorktreeResult{}, githubpkg.ErrNotConnected
	}
	token, err := gh.AccessToken()
	if err != nil {
		return CreateWorktreeResult{}, err
	}

	root, err := workRootDir()
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	worktreeULID, err := ulid.New(ulid.Now(), cryptoRand)
	if err != nil {
		return CreateWorktreeResult{}, fmt.Errorf("generate worktree id: %w", err)
	}
	worktreeID := worktreeULID.String()
	projectDir := filepath.Join(root, worktreeID)

	if _, statErr := os.Stat(projectDir); statErr == nil {
		return CreateWorktreeResult{}, fmt.Errorf("project dir %q already exists", projectDir)
	} else if !os.IsNotExist(statErr) {
		return CreateWorktreeResult{}, fmt.Errorf("check project dir %q: %w", projectDir, statErr)
	}

	cloneURL := fmt.Sprintf("%s/%s/%s.git", gitHubCloneBase, owner, repo)
	if err := s.importRepo(ctx, projectDir, cloneURL, token, worktreeID); err != nil {
		// Roll back so a retry doesn't trip the already-exists guard or
		// leave a half-cloned tree behind.
		if rmErr := os.RemoveAll(projectDir); rmErr != nil {
			s.log.Printf("warning: rollback remove project dir %s: %v", projectDir, rmErr)
		}
		return CreateWorktreeResult{}, err
	}

	branch, err := git.CurrentBranch(projectDir)
	if err != nil {
		if rmErr := os.RemoveAll(projectDir); rmErr != nil {
			s.log.Printf("warning: rollback remove project dir %s: %v", projectDir, rmErr)
		}
		return CreateWorktreeResult{}, fmt.Errorf("read checked-out branch: %w", err)
	}

	originRepo := owner + "/" + repo
	s.log.Printf("imported project %s (%q) at %s", worktreeID, originRepo, projectDir)
	return CreateWorktreeResult{
		WorktreeID:  worktreeID,
		Branch:      branch,
		WorktreeDir: projectDir,
		DisplayName: repo,
		OriginRepo:  originRepo,
	}, nil
}

// importRepo clones cloneURL into projectDir keeping its remote, gives it
// a local committer identity so later commits don't depend on global git
// config, and stamps the worktree id. Any error leaves cleanup to the
// caller.
func (s *Service) importRepo(ctx context.Context, projectDir, cloneURL, token, worktreeID string) error {
	if err := git.CloneShallowKeepRemote(ctx, cloneURL, projectDir, token); err != nil {
		return fmt.Errorf("clone repo: %w", err)
	}
	if err := git.SetLocalConfig(projectDir, "user.name", s.projectCommitterName); err != nil {
		return fmt.Errorf("set config user.name: %w", err)
	}
	if err := git.SetLocalConfig(projectDir, "user.email", s.projectCommitterEmail); err != nil {
		return fmt.Errorf("set config user.email: %w", err)
	}
	if err := agent.WriteLocalWorktreeID(projectDir, worktreeID); err != nil {
		return fmt.Errorf("stamp worktree-id: %w", err)
	}
	return nil
}
