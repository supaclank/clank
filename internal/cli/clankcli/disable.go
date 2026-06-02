package clankcli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
)

// disableCmd registers `clank disable` — opt this worktree out of clank
// push by clearing its cached worktree-id. The global autopush triggers
// stay installed but no-op for an untracked worktree.
func disableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disable [path]",
		Short: "Stop tracking this worktree for clank push",
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
			removed, err := agent.RemoveLocalWorktreeID(absRepo)
			if err != nil {
				return fmt.Errorf("untrack worktree: %w", err)
			}
			if removed {
				fmt.Fprintln(cmd.OutOrStdout(), "Stopped tracking this worktree — `clank push` will no longer sync it.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "This worktree wasn't tracked.")
			}
			return nil
		},
	}
	return cmd
}
