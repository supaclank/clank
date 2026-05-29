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
// worktree to clank-sync. With `--migrate` (or `-m`), also hand off
// ownership to the remote host.
//
// Idempotency: when the local SHAs match the remote's latest
// checkpoint, plain push exits early with "Already up to date". For
// `--migrate`, the no-op case is "remote-owned + in-sync" (already
// migrated). Divergence against a remote-owned worktree on `--migrate`
// is refused unless `--force` is also passed.
func pushCmd() *cobra.Command {
	var (
		baseURL  string
		token    string
		display  string
		repoPath string
		alsoMig  bool
		force    bool
		timing   bool
	)
	cmd := &cobra.Command{
		Use:   "push [repo-path]",
		Short: "Upload a checkpoint of a local worktree to clank-sync",
		Long: `Build a two-bundle checkpoint (HEAD history + uncommitted state)
of the repo at <repo-path> and upload it to clank-sync. The bundle
streams from the laptop directly to object storage via a presigned
URL — no bytes pass through clank-sync's process memory.

Worktree IDs are cached per-clone inside the repo's git dir at
$(git rev-parse --absolute-git-dir)/clank/worktree-id. First push
registers the worktree and caches the ID; linked worktrees from
` + "`git worktree add`" + ` get their own. Worktree ownership is
per-user (any laptop signed in with the same identity can push);
no per-device disambiguation.

With --migrate (or -m), after the upload completes, hand off worktree
ownership to the remote host. After this returns, the local laptop
is no longer the owner — to resume work locally, run
` + "`clank pull --migrate`" + `.

--force is only accepted alongside --migrate: it discards remote-side
changes when migrating onto a worktree the remote currently owns.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if force && !alsoMig {
				return errors.New("--force only applies with --migrate (-m)")
			}
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

			ctx := context.Background()

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

			// Branch on the matrix from the plan. The "early exit"
			// branches print a styled confirmation and return; the
			// fall-through cases proceed to the full push.
			if !alsoMig {
				return runPushNoMigrate(cmd, ctx, timer, cli, absRepo, worktreeID, parity)
			}
			return runPushMigrate(cmd, ctx, timer, cli, dc, absRepo, worktreeID, parity, force)
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", envOrDefault("CLANK_GATEWAY_URL", ""), "gateway base URL (default: active remote's gateway_url)")
	cmd.Flags().StringVar(&token, "token", envOrDefault("CLANK_SYNC_TOKEN", ""), "bearer token for the gateway (default: active remote's access_token)")
	cmd.Flags().StringVar(&display, "display-name", "", "display name for newly-registered worktrees (default: basename of repo-path)")
	cmd.Flags().BoolVarP(&alsoMig, "migrate", "m", false, "after pushing, also transfer worktree ownership to the remote host")
	cmd.Flags().BoolVar(&force, "force", false, "with --migrate, discard remote changes when migrating onto a remote-owned worktree")
	cmd.Flags().BoolVar(&timing, "timing", false, "print a per-phase timing breakdown to stderr (also enabled by CLANK_TIMING=1)")
	return cmd
}

// runPushNoMigrate handles `clank push` without --migrate. Plain push
// is permissive (mirrors git push to a non-owned branch): always
// uploads when local differs from remote, but logs an extra warning
// when the remote owner has unsynced changes that will need to be
// resolved on next --migrate.
func runPushNoMigrate(cmd *cobra.Command, ctx context.Context, timer *phaseTimer, cli *syncclient.Client, absRepo, worktreeID string, parity parityResult) error {
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

	if parity.OwnerKind == "remote" && parity.HasCheckpoint {
		fmt.Fprintln(cmd.OutOrStdout(), styleWarn.Render("⚠ Remote owner has changes you don't have locally."))
		fmt.Fprintln(cmd.OutOrStdout(), "  "+styleDim.Render("Resolve this on the next `clank push -m` or `clank pull -m`."))
	}
	return nil
}

// runPushMigrate handles `clank push --migrate`. Refuses on
// remote-owned + diverged unless --force. Skips entirely on the
// already-migrated no-op case.
func runPushMigrate(cmd *cobra.Command, ctx context.Context, timer *phaseTimer, cli *syncclient.Client, dc *daemonclient.Client, absRepo, worktreeID string, parity parityResult, force bool) error {
	// Already-migrated no-op: remote-owned + in-sync. The user is
	// running --migrate again on a worktree that's already where it
	// needs to be.
	if parity.OwnerKind == "remote" && parity.InSync {
		fmt.Fprintln(cmd.OutOrStdout(), styleOK.Render("✓ Already migrated")+styleDim.Render(" — worktree already remote-owned and in sync"))
		return nil
	}

	// Refuse the dangerous case (remote owns + diverged) unless
	// explicitly forced. The user's safer option is `pull -m` first.
	if parity.OwnerKind == "remote" && !parity.InSync && !force {
		printPushMigrateConflict(cmd)
		return errors.New("push --migrate refused: remote-owned worktree has unsynced changes (see options above)")
	}

	// Pre-flight version compatibility check. Same as today.
	localCli, err := daemonclient.NewLocalClient()
	if err != nil {
		return fmt.Errorf("local daemon client: %w", err)
	}
	done := timer.Start("version compatibility check")
	err = assertOpencodeCompatible(cmd.Context(), cmd.ErrOrStderr(), localCli, dc)
	done()
	if err != nil {
		return err
	}

	done = timer.Start("push checkpoint")
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

	mctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
	defer cancel()
	done = timer.Start("migrate worktree (gateway)")
	mres, err := dc.MigrateWorktree(mctx, worktreeID, daemonclient.MigrateToRemote)
	done()
	if err != nil {
		return fmt.Errorf("migrate to remote: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "migrated worktree %s → %s/%s\n",
		mres.WorktreeID, mres.NewOwnerKind, mres.NewOwnerID)
	return nil
}

func printPushMigrateConflict(cmd *cobra.Command) {
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, styleErr.Render("✗ Cannot migrate: the remote has changes you don't have locally."))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Options:")
	fmt.Fprintln(w, "    "+styleCmdHint.Render("clank pull -m")+"                 pull remote changes (default; mirrors `git pull`)")
	fmt.Fprintln(w, "    "+styleCmdHint.Render("clank push -m --force")+"         discard remote changes; push your state up")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+styleDim.Render("(Merge / rebase strategies coming in a future release.)"))
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
