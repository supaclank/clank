package host

// DELETE /repos/{slug} — whole-repo removal: every linked worktree
// (sessions purged, dirs removed through git) plus the canonical. Per
// CLAUDE.md's per-method-file rule this operation lives on its own.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	"github.com/oklog/ulid/v2"
)

// DeleteRepo removes the repo identified by slug: every linked worktree
// (session purge + busy guard + git-aware removal via
// removeLinkedWorktree) and then the canonical clone itself. Fails fast
// with ErrWorktreeBusy BEFORE deleting anything when any worktree has a
// live session; a session that starts mid-loop still trips the
// per-worktree re-check, leaving a partially-deleted repo the caller
// can retry (each leg is idempotent).
//
// LOCK ORDER: repo lock, then per-worktree locks inside
// removeLinkedWorktree — the same order DeleteWorktree uses, so the
// two can't ABBA-deadlock.
func (s *Service) DeleteRepo(ctx context.Context, slug string) error {
	repo, err := resolveRepoSlug(slug)
	if err != nil {
		return err
	}
	if repo.localCheckout {
		return fmt.Errorf("%w: %s", ErrCannotDeleteLocalCheckout, repo.gitDir)
	}
	gitDir := repo.gitDir

	defer s.lockRepo(repo.slug)()

	worktrees, err := git.ListWorktrees(gitDir)
	if err != nil {
		return err
	}
	type target struct{ id, path string }
	var targets []target
	for _, wt := range worktrees {
		if wt.Bare {
			continue
		}
		if _, statErr := os.Stat(wt.Path); statErr != nil {
			continue // prunable bookkeeping — gone with the canonical
		}
		id, idErr := agent.ReadLocalWorktreeID(wt.Path)
		if idErr != nil || id == "" {
			// Linked worktrees live outside the canonical dir (~/work/<ULID>),
			// so they do NOT go down with it below — fall back to the dir
			// name, which is the ULID the worktree was created under.
			base := filepath.Base(wt.Path)
			if _, perr := ulid.ParseStrict(base); perr != nil {
				s.log.Printf("delete repo %s: worktree %s has no readable id and dir name isn't a ULID: %v", slug, wt.Path, idErr)
				continue
			}
			id = base
		}
		targets = append(targets, target{id: id, path: wt.Path})
	}

	// Fail fast before any destruction.
	for _, t := range targets {
		busy, err := s.WorktreeHasActiveSession(ctx, t.id)
		if err != nil {
			return err
		}
		if busy {
			return ErrWorktreeBusy
		}
	}
	for _, t := range targets {
		if err := s.removeLinkedWorktree(ctx, t.id, t.path, gitDir); err != nil {
			return fmt.Errorf("delete worktree %s: %w", t.id, err)
		}
	}

	if err := os.RemoveAll(filepath.Dir(gitDir)); err != nil {
		return fmt.Errorf("remove canonical %s: %w", slug, err)
	}
	s.log.Printf("deleted repo %s (%d worktrees)", slug, len(targets))
	return nil
}
