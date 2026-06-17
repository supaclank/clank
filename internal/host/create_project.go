package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
)

const (
	// projectInitialBranch is the branch a freshly-scaffolded project
	// starts on, independent of the host's init.defaultBranch config.
	projectInitialBranch = "main"
	// projectInitialCommitMessage is the message of the single commit
	// that seeds a new project's history.
	projectInitialCommitMessage = "Initial commit"

	// defaultProjectCommitter{Name,Email} are the neutral fallback git
	// committer identity for a scaffolded project's seed commit when the
	// operator hasn't injected one (Options.ProjectCommitter*). Kept
	// vendor-neutral (example.com per RFC 2606) — real attribution is a
	// deploy-time concern, not an OSS constant.
	defaultProjectCommitterName  = "clank"
	defaultProjectCommitterEmail = "noreply@example.com"
)

// CreateProjectFromTemplate scaffolds a brand-new local project on this
// host by cloning cloneURL for its files, then giving it a fresh local
// history with no remote. The project lands at ~/work/<WorktreeID>/ — the
// same root materialized worktrees use — so sessions resolve it via a
// GitRef carrying only the worktree id.
//
// cloneURL is supplied by the gateway from its configured template
// catalog; the host treats it as opaque and never accepts a client-
// supplied URL directly. name becomes both the display name and the
// origin_repo label (there is no git remote to derive one from).
func (s *Service) CreateProjectFromTemplate(ctx context.Context, cloneURL, name string) (CreateWorktreeResult, error) {
	if cloneURL == "" {
		return CreateWorktreeResult{}, fmt.Errorf("%w: clone_url is required", ErrInvalidArgument)
	}
	if name == "" {
		return CreateWorktreeResult{}, fmt.Errorf("%w: name is required", ErrInvalidArgument)
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

	if err := s.scaffoldProject(ctx, projectDir, cloneURL, worktreeID); err != nil {
		// Roll back so a retry doesn't trip the already-exists guard above
		// or leave a half-cloned tree behind.
		if rmErr := os.RemoveAll(projectDir); rmErr != nil {
			s.log.Printf("warning: rollback remove project dir %s: %v", projectDir, rmErr)
		}
		return CreateWorktreeResult{}, err
	}

	s.log.Printf("created project %s (%q) at %s", worktreeID, name, projectDir)
	return CreateWorktreeResult{
		WorktreeID:  worktreeID,
		Branch:      projectInitialBranch,
		WorktreeDir: projectDir,
		DisplayName: name,
		OriginRepo:  name,
	}, nil
}

// scaffoldProject clones cloneURL into projectDir, replaces the cloned
// history with a single fresh commit (no remote), and stamps the
// worktree id. Any error leaves cleanup to the caller.
func (s *Service) scaffoldProject(ctx context.Context, projectDir, cloneURL, worktreeID string) error {
	if err := git.Clone(ctx, cloneURL, projectDir); err != nil {
		return fmt.Errorf("clone template: %w", err)
	}
	// Drop the template's history + origin so the new project is a clean,
	// remote-less local repo the agent owns from commit one.
	if err := os.RemoveAll(filepath.Join(projectDir, ".git")); err != nil {
		return fmt.Errorf("strip template git dir: %w", err)
	}
	if err := git.Init(ctx, projectDir, projectInitialBranch); err != nil {
		return fmt.Errorf("init repo: %w", err)
	}
	if err := git.SetLocalConfig(projectDir, "user.name", s.projectCommitterName); err != nil {
		return fmt.Errorf("set config user.name: %w", err)
	}
	if err := git.SetLocalConfig(projectDir, "user.email", s.projectCommitterEmail); err != nil {
		return fmt.Errorf("set config user.email: %w", err)
	}
	if err := git.AddAll(projectDir); err != nil {
		return fmt.Errorf("add files: %w", err)
	}
	if err := git.Commit(projectDir, projectInitialCommitMessage); err != nil {
		return fmt.Errorf("seed initial commit: %w", err)
	}
	if err := agent.WriteLocalWorktreeID(projectDir, worktreeID); err != nil {
		return fmt.Errorf("stamp worktree-id: %w", err)
	}
	return nil
}
