package clankcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// pullCmd registers `clank pull` — symmetric counterpart to push.
//
// `clank pull --migrate` (or `-m`) runs the gateway's two-phase
// migrate-back: materialize → apply locally → commit.
//
// Idempotency: when the local SHAs already match the remote's latest
// checkpoint and we already own the worktree, the command exits early
// with "Already up to date". When local-owned but local differs from
// remote, the pull is refused unless `--force` is also passed —
// `--force` discards local changes and resets to the remote.
func pullCmd() *cobra.Command {
	var (
		worktreeID string
		alsoMig    bool
		force      bool
		timing     bool
	)
	cmd := &cobra.Command{
		Use:   "pull [repo-path]",
		Short: "Pull the sandbox's latest state and reclaim ownership",
		Long: `Symmetric counterpart to ` + "`clank push`" + `.

With --migrate (or -m): the gateway wakes the sandbox, asks it to
checkpoint its current state, hands the laptop presigned GET URLs
for the bundles. The laptop downloads + applies the checkpoint
locally; on success, ownership atomically flips back to the laptop.
If apply fails, the sandbox keeps ownership so the user can retry.

--force is only accepted alongside --migrate: it discards local
changes when pulling onto a worktree the laptop already owns (i.e.
overwriting unsynced local work with the remote's last sync).

Without --migrate: bare data-only pull is post-MVP.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if force && !alsoMig {
				return errors.New("--force only applies with --migrate (-m)")
			}
			repoPath := ""
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
				return errors.New("bare `clank pull` is not implemented yet (post-MVP). Use `clank pull --migrate` (or -m) to download the sandbox's latest state and reclaim ownership")
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

			dc, err := daemonclient.NewRemoteClient()
			if err != nil {
				return fmt.Errorf("remote client: %w", err)
			}

			timer := newPhaseTimer(timing || envTrue("CLANK_TIMING"))
			defer timer.Summary(cmd.ErrOrStderr())

			// Snapshot + parity is a no-op fast-path. It needs a git
			// repo on disk; pulling into a fresh destination
			// (--worktree-id + new path) skips it and goes straight
			// to materialize — checkpoint.Apply will git-init the
			// target as part of restore.
			isRepo, err := isGitRepo(absRepo)
			if err != nil {
				return fmt.Errorf("check repo state: %w", err)
			}
			if isRepo {
				done := timer.Start("snapshot local")
				snap, err := snapshotRepo(cmd.Context(), absRepo)
				done()
				if err != nil {
					return fmt.Errorf("snapshot repo: %w", err)
				}

				done = timer.Start("parity check")
				parity, err := checkParity(cmd.Context(), dc, worktreeID, snap)
				done()
				if err != nil {
					return fmt.Errorf("check remote state: %w", err)
				}

				if parity.RemoteNotFound {
					return fmt.Errorf("worktree %s not registered on the active remote — run `clank push` to register it first", worktreeID)
				}

				// Already-reclaimed no-op: local-owned + in-sync.
				if parity.OwnerKind == "local" && parity.InSync {
					fmt.Fprintln(cmd.OutOrStdout(), styleOK.Render("✓ Already up to date")+styleDim.Render(" — you already own this worktree and local matches remote"))
					return nil
				}

				// Refuse the dangerous case (local owns + diverged) unless
				// explicitly forced. The user's safer option is push -m.
				if parity.OwnerKind == "local" && !parity.InSync && !force {
					printPullMigrateConflict(cmd)
					return errors.New("pull --migrate refused: local-owned worktree has unsynced changes (see options above)")
				}
			}

			return runPullMigrate(cmd, timer, dc, absRepo, worktreeID)
		},
	}
	cmd.Flags().BoolVarP(&alsoMig, "migrate", "m", false, "download the sandbox's checkpoint and reclaim ownership")
	cmd.Flags().BoolVar(&force, "force", false, "with --migrate, discard local changes when pulling onto a local-owned worktree")
	cmd.Flags().StringVar(&worktreeID, "worktree-id", "", "Worktree ID (default: read from <repo>/.clank/worktree-id)")
	cmd.Flags().BoolVar(&timing, "timing", false, "print a per-phase timing breakdown to stderr (also enabled by CLANK_TIMING=1)")
	return cmd
}

// runPullMigrate runs the full materialize → apply → commit flow.
// Reaches this only after the parity branching upstream has decided
// the operation is safe (or --force has been passed).
func runPullMigrate(cmd *cobra.Command, timer *phaseTimer, dc *daemonclient.Client, absRepo, worktreeID string) error {
	// Refuse the migration up-front on incompatible opencode versions
	// across hosts — cheaper to fail here than after the sprite has
	// done its export work. Local client is also needed downstream
	// for the session-apply step, so we construct it here once.
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

	// Phase 1: materialize. Independent budget — sandbox cold-start
	// + checkpoint can run minutes on its own. Derived from cmd.Context
	// so Ctrl+C / CLI cancellation propagates through every phase.
	materializeCtx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
	defer cancel()
	fmt.Fprintf(cmd.OutOrStdout(), "asking sandbox to checkpoint...\n")
	done = timer.Start("materialize (gateway)")
	mres, err := dc.MaterializeMigration(materializeCtx, worktreeID)
	done()
	if err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "checkpoint %s (HEAD %s); downloading...\n", mres.CheckpointID, shortSHA(mres.HeadCommit))

	applyCtx, applyCancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
	defer applyCancel()

	// Phase 2: download + apply locally
	done = timer.Start("apply code locally")
	err = applyRemoteCheckpoint(applyCtx, absRepo, mres)
	done()
	if err != nil {
		return fmt.Errorf("apply checkpoint locally: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "applied to %s\n", absRepo)

	// Phase 2b: import sessions via the local clank-host. Skipped when
	// the sprite had no opencode sessions in the worktree (empty
	// session_manifest_url). Failures here abort before commit.
	if mres.SessionManifestURL != "" {
		sessCtx, sessCancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		done = timer.Start("apply sessions locally")
		err := localCli.ApplySessionCheckpoint(sessCtx, worktreeID, mres.SessionManifestURL, mres.SessionBlobURLs)
		done()
		sessCancel()
		if err != nil {
			return fmt.Errorf("apply sessions locally: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "imported %d session(s) locally\n", len(mres.SessionBlobURLs))
	}

	fmt.Fprintf(cmd.OutOrStdout(), "committing ownership transfer...\n")

	// Phase 3: commit ownership transfer. Independent budget — the
	// apply phase above can burn most of applyCtx, leaving too little
	// to flip ownership cleanly.
	commitCtx, commitCancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
	defer commitCancel()
	done = timer.Start("commit migration")
	res, err := dc.CommitMigration(commitCtx, worktreeID, mres.CheckpointID, mres.MigrationToken)
	done()
	if err != nil {
		return fmt.Errorf("commit migration (apply succeeded; rerun pull to retry the ownership flip): %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "reclaimed worktree %s → %s/%s\n", res.WorktreeID, res.NewOwnerKind, res.NewOwnerID)
	return nil
}

func printPullMigrateConflict(cmd *cobra.Command) {
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, styleErr.Render("✗ Cannot migrate: your local state differs from the remote's last sync."))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Options:")
	fmt.Fprintln(w, "    "+styleCmdHint.Render("clank push -m")+"                 keep local changes; push them up")
	fmt.Fprintln(w, "    "+styleCmdHint.Render("clank pull -m --force")+"         discard local changes; reset to remote's state")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+styleDim.Render("(Merge / rebase strategies coming in a future release.)"))
}

// applyRemoteCheckpoint downloads the manifest + 2 bundles from the
// presigned URLs in the materialize response, then applies them into
// repoPath via checkpoint.Apply. Caller has already established
// repoPath; this overwrites its working tree to match the manifest.
func applyRemoteCheckpoint(ctx context.Context, repoPath string, mres *daemonclient.MaterializeResponse) error {
	cli := &http.Client{Timeout: 5 * time.Minute}

	// TODO(coderabbit): stream bundle bytes directly into checkpoint.Apply
	// instead of buffering in RAM. https://github.com/Acksell/clank/pull/17#discussion_r3227672576
	manifestBytes, err := fetchURL(ctx, cli, mres.ManifestURL)
	if err != nil {
		return fmt.Errorf("fetch manifest: %w", err)
	}
	var manifest checkpoint.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	headBytes, err := fetchURL(ctx, cli, mres.HeadCommitURL)
	if err != nil {
		return fmt.Errorf("fetch head bundle: %w", err)
	}
	incrBytes, err := fetchURL(ctx, cli, mres.IncrementalURL)
	if err != nil {
		return fmt.Errorf("fetch incremental bundle: %w", err)
	}
	return checkpoint.Apply(ctx, repoPath, &manifest, bytes.NewReader(headBytes), bytes.NewReader(incrBytes))
}

func fetchURL(ctx context.Context, cli *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return nil, fmt.Errorf("GET %d: %s", resp.StatusCode, string(preview))
	}
	return io.ReadAll(resp.Body)
}
