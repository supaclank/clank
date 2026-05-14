package clankcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
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
// worktree to clank-sync. With `--migrate`, also hand off ownership
// to the remote host so a session-create against this worktree
// resolves to the synced state on the remote (no clone, no SSH).
func pushCmd() *cobra.Command {
	var (
		baseURL  string
		token    string
		display  string
		repoPath string
		alsoMig  bool
		timing   bool
	)
	cmd := &cobra.Command{
		Use:   "push [repo-path]",
		Short: "Upload a checkpoint of a local worktree to clank-sync",
		Long: `Build a two-bundle checkpoint (HEAD history + uncommitted state)
of the repo at <repo-path> and upload it to clank-sync. The bundle
streams from the laptop directly to object storage via a presigned
URL — no bytes pass through clank-sync's process memory.

Worktree IDs are cached per-repo at <repo>/.clank/worktree-id.
First push registers the worktree and caches the ID. Worktree
ownership is per-user (any laptop signed in with the same identity
can push); no per-device disambiguation.

With --migrate, after the upload completes, hand off worktree
ownership to the remote host. After this returns, the local laptop
is no longer the owner — to resume work locally, run
` + "`clank pull --migrate`" + `.`,
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

			cli, err := syncclient.New(syncclient.Config{
				BaseURL:   baseURL,
				AuthToken: token,
			})
			if err != nil {
				return err
			}
			ctx := context.Background()

			timer := newPhaseTimer(timing || envTrue("CLANK_TIMING"))
			defer timer.Summary(cmd.ErrOrStderr())

			worktreeID, err := agent.ReadLocalWorktreeID(absRepo)
			if err != nil {
				return fmt.Errorf("load cached worktree id: %w", err)
			}
			if worktreeID == "" {
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

			// When --migrate is set, run the pre-flight check (version
			// compatibility + reachability of remote clank-host) BEFORE
			// PushCheckpoint. Otherwise a sprite that's unreachable or
			// has the wrong opencode version makes us waste a full
			// checkpoint upload first. Constructed clients are reused
			// downstream for the session leg + migrate call.
			var dc, localCli *daemonclient.Client
			if alsoMig {
				dc, err = daemonclient.NewRemoteClient()
				if err != nil {
					return fmt.Errorf("remote client: %w", err)
				}
				localCli, err = daemonclient.NewLocalClient()
				if err != nil {
					return fmt.Errorf("local daemon client: %w", err)
				}
				done := timer.Start("version compatibility check")
				err = assertOpencodeCompatible(cmd.Context(), cmd.ErrOrStderr(), localCli, dc)
				done()
				if err != nil {
					return err
				}
			}

			done := timer.Start("push checkpoint")
			res, err := cli.PushCheckpoint(ctx, worktreeID, absRepo)
			done()
			if err != nil {
				return fmt.Errorf("push checkpoint: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pushed checkpoint %s (HEAD %s)\n",
				res.CheckpointID, shortSHA(res.Manifest.HeadCommit))

			if alsoMig {
				// Session leg — ride the session blobs into the same
				// checkpoint so code + sessions transfer atomically. If
				// pushSessionLeg fails the migration aborts before
				// ownership flips (TransferOwnership runs on the
				// gateway side after the sprite-apply succeeds for
				// BOTH legs).
				if err := pushSessionLeg(cmd, timer, worktreeID, res.CheckpointID, cli); err != nil {
					return fmt.Errorf("push session leg: %w", err)
				}

				mctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				done = timer.Start("migrate worktree (gateway)")
				mres, err := dc.MigrateWorktree(mctx, worktreeID, daemonclient.MigrateToRemote)
				done()
				if err != nil {
					return fmt.Errorf("migrate to remote: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "migrated worktree %s → %s/%s\n",
					mres.WorktreeID, mres.NewOwnerKind, mres.NewOwnerID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", envOrDefault("CLANK_GATEWAY_URL", ""), "gateway base URL (default: active remote's gateway_url)")
	cmd.Flags().StringVar(&token, "token", envOrDefault("CLANK_SYNC_TOKEN", ""), "bearer token for the gateway (default: active remote's access_token)")
	cmd.Flags().StringVar(&display, "display-name", "", "display name for newly-registered worktrees (default: basename of repo-path)")
	cmd.Flags().BoolVar(&alsoMig, "migrate", false, "after pushing, also transfer worktree ownership to the remote host")
	cmd.Flags().BoolVar(&timing, "timing", false, "print a per-phase timing breakdown to stderr (also enabled by CLANK_TIMING=1)")
	return cmd
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
