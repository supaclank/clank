package clankcli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/clanksync/worktreescope"
)

// disableCmd registers `clank disable` — opt this worktree out of clank
// push by clearing its cached worktree-id. With --repo, undo `clank init`:
// turn off repo-wide auto-tracking and untrack every worktree. The global
// autopush triggers stay installed but no-op for an untracked worktree.
func disableCmd() *cobra.Command {
	var repo bool
	cmd := &cobra.Command{
		Use:   "disable [path]",
		Short: "Stop tracking this worktree (or, with --repo, the whole repo) for clank push",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			repoPath := "."
			if len(args) == 1 {
				repoPath = args[0]
			}
			absRepo, err := filepath.Abs(repoPath)
			if err != nil {
				return fmt.Errorf("resolve repo path: %w", err)
			}
			if repo {
				return runDisableRepo(cmd, absRepo)
			}
			return runDisableWorktree(cmd, absRepo)
		},
	}
	cmd.Flags().BoolVar(&repo, "repo", false, "stop auto-tracking the whole repo (undo `clank init`) and untrack all its worktrees")
	return cmd
}

// runDisableWorktree clears the current worktree's cached id. When the repo
// is still auto-tracked it warns that the next push re-registers it, so the
// user isn't surprised when tracking reappears.
func runDisableWorktree(cmd *cobra.Command, absRepo string) error {
	removed, err := agent.RemoveLocalWorktreeID(absRepo)
	if err != nil {
		return fmt.Errorf("untrack worktree: %w", err)
	}
	if removed {
		fmt.Fprintln(cmd.OutOrStdout(), "Stopped tracking this worktree — `clank push` will no longer sync it.")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "This worktree wasn't tracked.")
	}

	tracked, err := agent.IsRepoAutoTracked(absRepo)
	if err != nil {
		return fmt.Errorf("check repo auto-track: %w", err)
	}
	if tracked {
		fmt.Fprintln(cmd.OutOrStdout(), "Note: this repo auto-tracks every worktree, so the next push re-registers it. Run `clank disable --repo` to turn that off.")
	}
	return nil
}

// runDisableRepo undoes `clank init`: removes the repo-wide auto-track
// marker and clears the cached id of every worktree in the repo.
func runDisableRepo(cmd *cobra.Command, absRepo string) error {
	disabled, err := agent.DisableRepoAutoTrack(absRepo)
	if err != nil {
		return fmt.Errorf("disable repo auto-track: %w", err)
	}

	scopes, err := worktreescope.WorktreesForRepo(absRepo, worktreescope.DefaultRecencyWindow)
	if err != nil {
		return fmt.Errorf("enumerate worktrees: %w", err)
	}
	var untracked int
	for _, s := range scopes {
		removed, err := agent.RemoveLocalWorktreeID(s.Path)
		if err != nil {
			return fmt.Errorf("untrack worktree %s: %w", s.Path, err)
		}
		if removed {
			untracked++
		}
	}

	if !disabled && untracked == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "This repo wasn't tracked.")
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Stopped auto-tracking this repo — cleared %d worktree(s). `clank push` will no longer sync them.\n", untracked)
	return nil
}
