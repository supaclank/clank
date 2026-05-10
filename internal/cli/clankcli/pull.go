package clankcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// pullCmd registers `clank pull` — symmetric counterpart to push.
//
// Bare `clank pull` (data-only download from clank-sync) is reserved
// for the day a remote host pushes its own checkpoints (the P4
// sprite-side push). Until then, only `clank pull --migrate` is
// useful: it reclaims worktree ownership from the remote so the
// local laptop can write again.
func pullCmd() *cobra.Command {
	var (
		repoPath   string
		worktreeID string
		deviceID   string
		alsoMig    bool
	)
	cmd := &cobra.Command{
		Use:   "pull [repo-path]",
		Short: "Pull a remote checkpoint to the local worktree (and optionally reclaim ownership)",
		Long: `Symmetric counterpart to ` + "`clank push`" + `.

With --migrate: reclaim worktree ownership from the remote host so
the local laptop can write checkpoints again. Today this is the
"Keep local" semantic — sandbox-side filesystem changes are
abandoned. The "Pull from sandbox" variant (download the remote's
latest checkpoint and apply it locally before reclaiming) is the
day's-out P4 work.

Without --migrate: data-only pull is not yet implemented. Until a
remote host can push its own checkpoints, there is nothing for the
laptop to pull beyond what it already has.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				repoPath = args[0]
			}
			if repoPath == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
				repoPath = cwd
			}
			absRepo, err := filepath.Abs(repoPath)
			if err != nil {
				return fmt.Errorf("resolve repo path: %w", err)
			}

			if !alsoMig {
				return fmt.Errorf("data-only pull is not yet implemented; pass --migrate to reclaim worktree ownership from the remote")
			}

			if worktreeID == "" {
				worktreeID, err = agent.ReadLocalWorktreeID(absRepo)
				if err != nil {
					return fmt.Errorf("load cached worktree id: %w", err)
				}
				if worktreeID == "" {
					return fmt.Errorf("no worktree id cached at %s/.clank/worktree-id — pass --worktree-id explicitly", absRepo)
				}
			}
			if deviceID == "" {
				deviceID, err = ensureDeviceID()
				if err != nil {
					return fmt.Errorf("device id: %w", err)
				}
			}

			dc, err := daemonclient.NewDefaultClient()
			if err != nil {
				return fmt.Errorf("daemon client: %w", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			res, err := dc.MigrateWorktree(ctx, worktreeID, deviceID, daemonclient.MigrateToLocal)
			if err != nil {
				return fmt.Errorf("migrate to local: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "reclaimed worktree %s → %s/%s\n",
				res.WorktreeID, res.NewOwnerKind, res.NewOwnerID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&alsoMig, "migrate", false, "reclaim worktree ownership from the remote host")
	cmd.Flags().StringVar(&worktreeID, "worktree-id", "", "Worktree ID (default: read from <repo>/.clank/worktree-id)")
	cmd.Flags().StringVar(&deviceID, "device-id", envOrDefault("CLANK_DEVICE_ID", ""), "device id (default: ~/.config/clank/device-id)")
	return cmd
}
