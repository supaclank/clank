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
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/repolabel"
	"github.com/acksell/clank/pkg/sessionsync"
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
		clean    bool
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

			// Sign in (active remote, or prompt on a TTY) and resolve a
			// sync client; --token/--base-url override for self-hosted/CI.
			cli, err := ensureLoggedIn(ctx, cmd, baseURL, token)
			if err != nil {
				return err
			}

			timer := newPhaseTimer(timing || envTrue("CLANK_TIMING"))
			defer timer.Summary(cmd.ErrOrStderr())

			// Resolve (and if needed register) the worktree id: auto-track
			// under AutoPushAllRepos, offer to track on a TTY, else error.
			worktreeID, err := ensureTracked(ctx, cmd, cli, absRepo, display, isInteractive(cmd))
			if err != nil {
				return err
			}

			// Serialize concurrent pushes for this worktree (e.g. a Claude
			// Stop hook and an opencode idle plugin firing at once) so they
			// don't race the checkpoint. Non-blocking: a contended push
			// exits quietly.
			gitDir, err := agent.GitDir(absRepo)
			if err != nil {
				return fmt.Errorf("resolve git dir: %w", err)
			}
			isLocked, release, err := pushlock.Acquire(gitDir)
			if err != nil {
				return fmt.Errorf("acquire push lock: %w", err)
			}
			if !isLocked {
				fmt.Fprintln(cmd.OutOrStdout(), "another push is already in progress for this worktree; skipping")
				return nil
			}
			defer release()

			// Build a Snapshot of local state and query the remote for
			// the latest synced checkpoint. The pair drives the
			// idempotency / divergence branches below; built before any
			// expensive work so we can fast-path no-op runs.
			done := timer.Start("snapshot local")
			snap, err := snapshotRepo(ctx, absRepo, clean)
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

			return runPush(cmd, ctx, timer, cli, absRepo, worktreeID, parity, clean)
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", envOrDefault("CLANK_GATEWAY_URL", ""), "gateway base URL (default: active remote's gateway_url)")
	cmd.Flags().StringVar(&token, "token", envOrDefault("CLANK_SYNC_TOKEN", ""), "bearer token for the gateway (default: active remote's access_token)")
	cmd.Flags().StringVar(&display, "display-name", "", "display name for newly-registered worktrees (default: basename of repo-path)")
	cmd.Flags().BoolVar(&timing, "timing", false, "print a per-phase timing breakdown to stderr (also enabled by CLANK_TIMING=1)")
	cmd.Flags().BoolVar(&clean, "clean", false, "push committed history only — ignore uncommitted/staged/untracked changes")
	return cmd
}

// runPush uploads the worktree's code checkpoint + opencode sessions.
// Fast-paths a no-op when local already matches the remote's latest
// checkpoint.
func runPush(cmd *cobra.Command, ctx context.Context, timer *phaseTimer, cli *syncclient.Client, absRepo, worktreeID string, parity parityResult, committedOnly bool) error {
	if parity.InSync {
		// Code matches the remote, but sessions sync on an INDEPENDENT axis:
		// a session can change (or its Claude transcript mtime bump on a bare
		// `--resume`) with no code change. A code-only early return would
		// leave such a session stuck — `clank status` flags it, yet push
		// would forever report "up to date" and never carry it.
		return pushSessionsWhenCodeInSync(cmd, ctx, timer, cli, absRepo, parity, committedOnly)
	}

	// On a TTY, one live status line spans the WHOLE push — build → upload
	// → save → sessions — driven by a ticker so it keeps animating even
	// while the server commits or sessions export. Non-interactive callers
	// (autopush hooks) get a nil observer, so nothing is drawn into logs.
	var ui *pushUI
	var obs syncclient.PushObserver
	if isInteractive(cmd) {
		ui = newPushUI(cmd.OutOrStdout(), remoteLabel(cli.BaseURL()))
		ui.start()
		obs = ui
	}

	// parity.RemoteHead is the server's last-synced HEAD — the base for an
	// incremental head bundle when our HEAD has advanced.
	res, err := cli.PushCheckpoint(ctx, worktreeID, absRepo, parity.RemoteHead, committedOnly, obs)
	if errors.Is(err, syncclient.ErrWorktreeNotRegistered) {
		// Stale local id: the worktree was deleted on the remote. Pause the
		// status line so the re-register notice prints cleanly, then retry
		// once with a full push (the fresh worktree has no synced HEAD).
		ui.finish()
		worktreeID, err = reregisterStaleWorktree(cmd, ctx, timer, cli, absRepo)
		if err != nil {
			return err
		}
		ui.start()
		res, err = cli.PushCheckpoint(ctx, worktreeID, absRepo, "", committedOnly, obs)
	}
	if err != nil {
		ui.finish()
		return fmt.Errorf("push checkpoint: %w", err)
	}
	// Surface the push's sub-step timings (build / per-blob upload / commit)
	// under --timing, so a slow push can be attributed precisely.
	for _, st := range res.Timings {
		timer.Record(st.Name, st.Duration)
	}

	// Session leg shares the same status line (Phase resets the byte bar);
	// obs drives the live (i/N) counter as each session uploads.
	ui.Phase(phaseSyncingSessions)
	exported, skipped, serr := pushSessions(ctx, timer, absRepo, res.CheckpointID, cli, obs)

	ui.finish() // tear the active line down; committed steps stay on screen
	if serr != nil {
		return fmt.Errorf("push session leg: %w", serr)
	}

	reportSkippedSessions(cmd, skipped)
	if exported > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "%s Synced %d session(s)\n", styleOK.Render("✓"), exported)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s Pushed checkpoint %s (HEAD %s) to %s\n",
		styleOK.Render("✓"), res.CheckpointID, shortSHA(res.Manifest.HeadCommit), remoteLabel(cli.BaseURL()))
	return nil
}

// pushSessionsWhenCodeInSync syncs unsynced sessions against the EXISTING
// latest checkpoint when the code is already in sync. Sessions sync on an
// axis independent of code, so an unsynced session must still be carried
// here rather than left stuck behind a "✓ Already up to date" no-op. When
// no session is unsynced (or the axis is undeterminable), it reports up to
// date — matching `clank status`, which uses the same unsynced check.
func pushSessionsWhenCodeInSync(cmd *cobra.Command, ctx context.Context, timer *phaseTimer, cli *syncclient.Client, absRepo string, parity parityResult, committedOnly bool) error {
	unsynced, known := unsyncedSessions(ctx, absRepo)
	if !known || len(unsynced) == 0 || parity.CheckpointID == "" {
		printAlreadyUpToDate(cmd, committedOnly)
		return nil
	}

	// Sessions are the only thing being pushed here, so drive the same live
	// "Syncing sessions (i/N)" line as the full push. Non-interactive callers
	// get a nil observer (no UI), matching runPush.
	var ui *pushUI
	var obs syncclient.PushObserver
	if isInteractive(cmd) {
		ui = newPushUI(cmd.OutOrStdout(), remoteLabel(cli.BaseURL()))
		ui.start()
		ui.Phase(phaseSyncingSessions)
		obs = ui
	}

	exported, skipped, err := pushSessions(ctx, timer, absRepo, parity.CheckpointID, cli, obs)
	ui.finish()
	if err != nil {
		return fmt.Errorf("push session leg: %w", err)
	}
	reportSkippedSessions(cmd, skipped)
	if exported == 0 {
		printAlreadyUpToDate(cmd, committedOnly)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s Synced %d session(s) (code already up to date)\n", styleOK.Render("✓"), exported)
	return nil
}

func printAlreadyUpToDate(cmd *cobra.Command, committedOnly bool) {
	detail := " (local state matches remote's latest checkpoint)"
	if committedOnly {
		detail = " (committed state matches remote; uncommitted changes ignored)"
	}
	fmt.Fprintln(cmd.OutOrStdout(), styleOK.Render("✓ Already up to date")+styleDim.Render(detail))
}

func reportSkippedSessions(cmd *cobra.Command, skipped []sessionsync.SkippedSession) {
	for _, sk := range skipped {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s skipped session %s: %s\n", styleDim.Render("•"), sk.ExternalID, sk.Reason)
	}
}

// reregisterStaleWorktree re-registers absRepo after the remote reported
// the cached worktree-id is unknown (the worktree was deleted upstream),
// rewrites the on-disk id cache, and returns the fresh id. Removes the
// manual `rm -r .git/clank && clank init` recovery dance.
func reregisterStaleWorktree(cmd *cobra.Command, ctx context.Context, timer *phaseTimer, cli *syncclient.Client, absRepo string) (string, error) {
	name := filepath.Base(absRepo)
	fmt.Fprintf(cmd.ErrOrStderr(), "worktree no longer exists on the remote; re-registering %q…\n", name)
	done := timer.Start("re-register worktree")
	id, err := cli.RegisterWorktree(ctx, name, repolabel.ComputeRepoLabel(absRepo))
	done()
	if err != nil {
		return "", fmt.Errorf("re-register worktree: %w", err)
	}
	if err := agent.WriteLocalWorktreeID(absRepo, id); err != nil {
		return "", fmt.Errorf("cache worktree id: %w", err)
	}
	return id, nil
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
