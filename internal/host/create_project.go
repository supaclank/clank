package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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

// CreateProjectFromTemplate scaffolds a brand-new project under the
// repo-first layout: a bare canonical at ~/work/repos/<slug>/repo.git
// seeded with one fresh commit of the template's files (no template
// history, no remote), plus a linked `git worktree` for main at
// ~/work/<WorktreeID>. A greenfield app is a REAL git repo from birth —
// branches, forks, and the repo overview all work before it's ever
// published; PublishToRemote later just adds origin + pushes.
//
// The seed commit is built in a throwaway temp checkout and pushed into
// the bare canonical over the filesystem — `git worktree add --orphan`
// would be simpler but needs git ≥ 2.42, newer than our runtime floor.
//
// cloneURL is supplied by the gateway from its configured template
// catalog; the host treats it as opaque and never accepts a client-
// supplied URL directly. name becomes the display name, the label, and
// (sanitized) the slug.
func (s *Service) CreateProjectFromTemplate(ctx context.Context, cloneURL, name string) (CreateWorktreeResult, error) {
	if cloneURL == "" {
		return CreateWorktreeResult{}, fmt.Errorf("%w: clone_url is required", ErrInvalidArgument)
	}
	if name == "" {
		return CreateWorktreeResult{}, fmt.Errorf("%w: name is required", ErrInvalidArgument)
	}

	slug, err := slugForName(name)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	gitDir, err := canonicalGitDir(slug)
	if err != nil {
		return CreateWorktreeResult{}, err
	}

	defer s.lockRepo(slug)()

	if err := s.scaffoldCanonical(ctx, gitDir, cloneURL, name); err != nil {
		// Roll back so a retry doesn't find a half-initialized canonical
		// (slugForName would otherwise suffix -2 on the corpse).
		if rmErr := os.RemoveAll(filepath.Dir(gitDir)); rmErr != nil {
			s.log.Printf("warning: rollback canonical %s: %v", gitDir, rmErr)
		}
		return CreateWorktreeResult{}, err
	}

	result, err := s.addRepoWorktree(ctx, slug, gitDir, projectInitialBranch, name)
	if err != nil {
		if rmErr := os.RemoveAll(filepath.Dir(gitDir)); rmErr != nil {
			s.log.Printf("warning: rollback canonical %s: %v", gitDir, rmErr)
		}
		return CreateWorktreeResult{}, err
	}

	s.log.Printf("created project %s (%q) → worktree %s", slug, name, result.WorktreeID)
	return result.CreateWorktreeResult, nil
}

// scaffoldCanonical builds the greenfield canonical: an empty bare repo
// seeded — via a temp non-bare checkout and a filesystem push — with a
// single commit of the template's files. The canonical gets the
// committer identity (shared by all its worktrees' commits), the
// display label, and the credential helper (dormant until publish wires
// an origin, then agent-run git + future fetches authenticate).
func (s *Service) scaffoldCanonical(ctx context.Context, gitDir, cloneURL, name string) error {
	if err := os.MkdirAll(filepath.Dir(gitDir), 0o755); err != nil {
		return fmt.Errorf("create canonical dir: %w", err)
	}
	if err := git.InitBare(ctx, gitDir, projectInitialBranch); err != nil {
		return err
	}
	if err := git.SetLocalConfig(gitDir, "user.name", s.projectCommitterName); err != nil {
		return fmt.Errorf("set config user.name: %w", err)
	}
	if err := git.SetLocalConfig(gitDir, "user.email", s.projectCommitterEmail); err != nil {
		return fmt.Errorf("set config user.email: %w", err)
	}
	if err := git.SetLocalConfig(gitDir, repoConfigLabelKey, name); err != nil {
		return fmt.Errorf("set config %s: %w", repoConfigLabelKey, err)
	}
	if helper := s.credentialHelperValue(); helper != "" {
		if err := git.SetLocalConfig(gitDir, "credential.helper", helper); err != nil {
			return fmt.Errorf("set config credential.helper: %w", err)
		}
	}

	// Seed commit: template files → one fresh commit in a temp checkout →
	// filesystem push into the bare canonical (no network, no auth).
	tmp, err := os.MkdirTemp("", "clank-scaffold-*")
	if err != nil {
		return fmt.Errorf("create scaffold temp dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			s.log.Printf("warning: remove scaffold temp dir %s: %v", tmp, rmErr)
		}
	}()
	seed := filepath.Join(tmp, "seed")
	if err := git.Clone(ctx, cloneURL, seed); err != nil {
		// Full git error (which echoes the clone URL, possibly with
		// embedded credentials) goes to the server log ONLY; the
		// returned error is a sanitized sentinel the mux maps to a
		// typed, client-safe response instead of an opaque 500.
		s.log.Printf("create-project: clone template failed: %v", err)
		return fmt.Errorf("%w: git clone failed — check that the template repository exists and is reachable", ErrTemplateCloneFailed)
	}
	// Drop the template's history + origin so the new project is a clean
	// local repo the agent owns from commit one.
	if err := os.RemoveAll(filepath.Join(seed, ".git")); err != nil {
		return fmt.Errorf("strip template git dir: %w", err)
	}
	if err := git.Init(ctx, seed, projectInitialBranch); err != nil {
		return fmt.Errorf("init seed repo: %w", err)
	}
	if err := git.SetLocalConfig(seed, "user.name", s.projectCommitterName); err != nil {
		return fmt.Errorf("set seed user.name: %w", err)
	}
	if err := git.SetLocalConfig(seed, "user.email", s.projectCommitterEmail); err != nil {
		return fmt.Errorf("set seed user.email: %w", err)
	}
	if err := git.AddAll(seed); err != nil {
		return fmt.Errorf("add template files: %w", err)
	}
	if err := git.Commit(seed, projectInitialCommitMessage); err != nil {
		return fmt.Errorf("seed initial commit: %w", err)
	}
	refspec := projectInitialBranch + ":refs/heads/" + projectInitialBranch
	if err := git.Push(seed, gitDir, refspec, git.PushOptions{}); err != nil {
		return fmt.Errorf("push seed commit into canonical: %w", err)
	}
	return nil
}
