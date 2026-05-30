package clankcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/clanksync/pushlock"
	"github.com/acksell/clank/internal/cloud"
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// pushCmd registers `clank push` — upload a checkpoint of the local
// worktree (code + opencode sessions) to clank-sync.
//
// Idempotency: when the local SHAs match the remote's latest
// checkpoint, push exits early with "Already up to date".
func pushCmd() *cobra.Command {
	var (
		baseURL  string
		token    string
		display  string
		repoPath string
		timing   bool
	)
	cmd := &cobra.Command{
		Use:   "push [repo-path]",
		Short: "Upload a checkpoint of a local worktree to clank-sync",
		Long: `Build a two-bundle checkpoint (HEAD history + uncommitted state) of
the repo at <repo-path> and upload it to clank-sync, along with the
worktree's opencode sessions. The bundle streams from the laptop
directly to object storage via a presigned URL — no bytes pass through
clank-sync's process memory.

The worktree must be tracked first (run ` + "`clank init`" + `); its ID is
cached inside the repo's git dir at
$(git rev-parse --absolute-git-dir)/clank/worktree-id. Linked worktrees
from ` + "`git worktree add`" + ` are tracked individually.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
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

			// Bound the whole push. Blob uploads no longer carry a
			// ResponseHeaderTimeout (an S3 PUT's headers arrive only after
			// the body lands), so a truly-stuck transfer must be capped
			// here rather than hanging forever. Generous enough for a large
			// bundle over a slow link.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			// Refresh BEFORE the first authenticated call so the sync
			// client below sees a fresh bearer. Without this, expired
			// access tokens silently produce 401 from RegisterWorktree.
			//
			// Skip when the caller supplied --token / CLANK_SYNC_TOKEN
			// explicitly — that bearer is independent of the active
			// remote profile (e.g. self-hosted static bearer, CI cron),
			// so refreshing the profile's expired refresh_token would
			// abort the push despite the caller having valid creds.
			if token == "" {
				if err := daemonclient.EnsureFreshActiveRemote(ctx); err != nil {
					if errors.Is(err, cloud.ErrUnauthorized) {
						return fmt.Errorf("session expired — run `clank login` to sign in again")
					}
					return fmt.Errorf("refresh remote session: %w", err)
				}
			}

			if baseURL == "" || token == "" {
				prefs, err := config.LoadPreferences()
				if err != nil {
					return fmt.Errorf("load preferences: %w", err)
				}
				if p := prefs.ActiveRemote(); p != nil {
					if baseURL == "" {
						baseURL = p.GatewayURL
					}
					if token == "" {
						token = p.AccessToken
					}
				}
			}
			if baseURL == "" {
				return fmt.Errorf("--base-url is required (or set CLANK_GATEWAY_URL, or configure an active remote via `clank remote add`)")
			}
			if token == "" {
				return fmt.Errorf("not signed in — run `clank login` to sign in to the active remote")
			}

			cli, err := syncclient.New(syncclient.Config{
				BaseURL:   baseURL,
				AuthToken: token,
			})
			if err != nil {
				return err
			}

			timer := newPhaseTimer(timing || envTrue("CLANK_TIMING"))
			defer timer.Summary(cmd.ErrOrStderr())

			worktreeID, err := agent.ReadLocalWorktreeID(absRepo)
			if err != nil {
				return fmt.Errorf("load cached worktree id: %w", err)
			}
			if worktreeID == "" {
				// Untracked. Require explicit opt-in unless the user turned
				// on global auto-push, in which case register on the fly.
				prefs, err := config.LoadPreferences()
				if err != nil {
					return fmt.Errorf("load preferences: %w", err)
				}
				if !prefs.AutoPushAllRepos {
					return fmt.Errorf("this worktree isn't tracked — run `clank init` (or `clank init --global` to auto-track every repo)")
				}
				name := display
				if name == "" {
					name = filepath.Base(absRepo)
				}
				done := timer.Start("register worktree")
				worktreeID, err = cli.RegisterWorktree(ctx, name)
				done()
				if err != nil {
					return fmt.Errorf("register worktree: %w", err)
				}
				if err := agent.WriteLocalWorktreeID(absRepo, worktreeID); err != nil {
					return fmt.Errorf("cache worktree id: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "registered worktree %s as %q\n", worktreeID, name)
			}

			// Serialize concurrent pushes for this worktree (e.g. a Claude
			// Stop hook and an opencode idle plugin firing at once) so they
			// don't race the checkpoint. Non-blocking: a contended push
			// exits quietly.
			gitDir, err := agent.GitDir(absRepo)
			if err != nil {
				return fmt.Errorf("resolve git dir: %w", err)
			}
			locked, release, err := pushlock.Acquire(gitDir)
			if err != nil {
				return fmt.Errorf("acquire push lock: %w", err)
			}
			if !locked {
				fmt.Fprintln(cmd.OutOrStdout(), "another push is already in progress for this worktree; skipping")
				return nil
			}
			defer release()

			// Build a Snapshot of local state and query the remote for
			// the latest synced checkpoint. The pair drives the
			// idempotency / divergence branches below; built before any
			// expensive work so we can fast-path no-op runs.
			done := timer.Start("snapshot local")
			snap, err := snapshotRepo(ctx, absRepo)
			done()
			if err != nil {
				return fmt.Errorf("snapshot repo: %w", err)
			}

			dc, err := daemonclient.NewRemoteClient()
			if err != nil {
				return fmt.Errorf("remote client: %w", err)
			}

			done = timer.Start("parity check")
			parity, err := checkParity(ctx, dc, worktreeID, snap)
			done()
			if err != nil {
				return fmt.Errorf("check remote state: %w", err)
			}

			return runPush(cmd, ctx, timer, cli, absRepo, worktreeID, parity)
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", envOrDefault("CLANK_GATEWAY_URL", ""), "gateway base URL (default: active remote's gateway_url)")
	cmd.Flags().StringVar(&token, "token", envOrDefault("CLANK_SYNC_TOKEN", ""), "bearer token for the gateway (default: active remote's access_token)")
	cmd.Flags().StringVar(&display, "display-name", "", "display name for newly-registered worktrees (default: basename of repo-path)")
	cmd.Flags().BoolVar(&timing, "timing", false, "print a per-phase timing breakdown to stderr (also enabled by CLANK_TIMING=1)")
	return cmd
}

// runPush uploads the worktree's code checkpoint + opencode sessions.
// Fast-paths a no-op when local already matches the remote's latest
// checkpoint.
func runPush(cmd *cobra.Command, ctx context.Context, timer *phaseTimer, cli *syncclient.Client, absRepo, worktreeID string, parity parityResult) error {
	if parity.InSync {
		fmt.Fprintln(cmd.OutOrStdout(), styleOK.Render("✓ Already up to date")+styleDim.Render(" (local state matches remote's latest checkpoint)"))
		return nil
	}

	done := timer.Start("push checkpoint")
	res, err := cli.PushCheckpoint(ctx, worktreeID, absRepo)
	done()
	if err != nil {
		return fmt.Errorf("push checkpoint: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "pushed checkpoint %s (HEAD %s)\n",
		res.CheckpointID, shortSHA(res.Manifest.HeadCommit))

	if err := pushSessionLeg(cmd, timer, absRepo, res.CheckpointID, cli); err != nil {
		return fmt.Errorf("push session leg: %w", err)
	}
	return nil
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
