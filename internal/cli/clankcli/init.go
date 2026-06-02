package clankcli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/clanksync/worktreescope"
	"github.com/acksell/clank/internal/cloud"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/repolabel"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// initCmd registers `clank init` — opt this repo into `clank push` by
// registering its recently-active worktrees with the active remote and
// installing the autopush triggers. With --global, instead enable
// auto-push for every repo.
func initCmd() *cobra.Command {
	var global bool
	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Track this repo for clank push (auto-push on idle)",
		Long: `Register the repo's recently-active worktrees with the active remote
so ` + "`clank push`" + ` can sync them, and install the autopush triggers
(a Claude Code hook + an opencode plugin) that push on idle. Run once
per repo, after ` + "`clank login`" + `.

With --global, enable auto-push for every repo instead — any repo you
open a session in will push to your active remote (worktree-ids are
registered on first push). Pushing throwaway/cloned repos is the cost.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if global {
				return runInitGlobal(cmd)
			}
			repoPath := "."
			if len(args) == 1 {
				repoPath = args[0]
			}
			return runInitRepo(cmd, repoPath)
		},
	}
	cmd.Flags().BoolVar(&global, "global", false, "auto-track every repo using the active remote (no per-repo init)")
	return cmd
}

func runInitGlobal(cmd *cobra.Command) error {
	if err := config.UpdatePreferences(func(p *config.Preferences) {
		p.AutoPushAllRepos = true
	}); err != nil {
		return fmt.Errorf("save preference: %w", err)
	}
	if err := ensureHarnessTriggers(cmd, isInteractive(cmd)); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Auto-push enabled for all repositories — any repo you work in pushes to your active remote on idle.")
	return nil
}

func runInitRepo(cmd *cobra.Command, repoPath string) error {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve repo path: %w", err)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()

	cli, err := activeRemoteSyncClient(ctx)
	if err != nil {
		return err
	}

	scopes, err := worktreescope.WorktreesForRepo(absRepo, worktreescope.DefaultRecencyWindow)
	if err != nil {
		return fmt.Errorf("enumerate worktrees: %w", err)
	}

	var registered, already int
	for _, s := range scopes {
		if s.WorktreeID != "" {
			already++
			continue
		}
		// Only auto-track recently-active worktrees; stale, never-tracked
		// ones are skipped so `clank init` doesn't register a pile of
		// abandoned branches.
		if !s.IsRecentlyActive {
			continue
		}
		id, err := cli.RegisterWorktree(ctx, filepath.Base(s.Path), repolabel.ComputeRepoLabel(s.Path))
		if err != nil {
			return fmt.Errorf("register worktree %s: %w", s.Path, err)
		}
		if err := agent.WriteLocalWorktreeID(s.Path, id); err != nil {
			return fmt.Errorf("cache worktree id for %s: %w", s.Path, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "tracking %s (%s) → %s\n", s.Path, s.Branch, id)
		registered++
	}

	if err := ensureHarnessTriggers(cmd, isInteractive(cmd)); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Initialized clank push: %d newly tracked, %d already tracked. Sessions auto-push on idle.\n", registered, already)
	return nil
}

// activeRemoteSyncClient builds a sync client for the active remote,
// refreshing its token first. Mirrors the resolution in `clank push`.
func activeRemoteSyncClient(ctx context.Context) (*syncclient.Client, error) {
	if err := daemonclient.EnsureFreshActiveRemote(ctx); err != nil {
		if errors.Is(err, cloud.ErrUnauthorized) {
			return nil, fmt.Errorf("session expired — run `clank login` to sign in again")
		}
		return nil, fmt.Errorf("refresh remote session: %w", err)
	}
	prefs, err := config.LoadPreferences()
	if err != nil {
		return nil, fmt.Errorf("load preferences: %w", err)
	}
	p := prefs.ActiveRemote()
	if p == nil || p.GatewayURL == "" {
		return nil, fmt.Errorf("no active remote configured — run `clank login` (and `clank remote add` if needed)")
	}
	if p.AccessToken == "" {
		return nil, fmt.Errorf("not signed in — run `clank login`")
	}
	return syncclient.New(syncclient.Config{BaseURL: p.GatewayURL, AuthToken: p.AccessToken})
}
