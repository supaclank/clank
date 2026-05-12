package clankcli

import (
	"bytes"
	"context"
	"encoding/json"
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
// `clank pull --migrate` runs the gateway's two-phase migrate-back:
//
//  1. Materialize: wake the sandbox, capture its current state as a new
//     checkpoint, return presigned GET URLs + a signed migration token.
//  2. Apply locally: download manifest + bundles, restore the working
//     tree via checkpoint.Apply.
//  3. Commit: send the migration token back to the gateway; ownership
//     atomically flips to the laptop.
//
// On apply failure between phases 2 and 3, ownership stays with the
// sandbox so the user can retry or recover.
//
// Bare `clank pull` (no `--migrate`) is post-MVP.
func pullCmd() *cobra.Command {
	var (
		repoPath   string
		worktreeID string
		alsoMig    bool
	)
	cmd := &cobra.Command{
		Use:   "pull [repo-path]",
		Short: "Pull the sandbox's latest state and reclaim ownership",
		Long: `Symmetric counterpart to ` + "`clank push`" + `.

With --migrate: the gateway wakes the sandbox, asks it to checkpoint
its current state, hands the laptop presigned GET URLs for the bundles.
The laptop downloads + applies the checkpoint locally; on success,
ownership atomically flips back to the laptop. If apply fails, the
sandbox keeps ownership so the user can retry.

Without --migrate: bare data-only pull is post-MVP.`,
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
				return fmt.Errorf("bare `clank pull` is not implemented yet (post-MVP). Use `clank pull --migrate` to download the sandbox's latest state and reclaim ownership")
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

			// Phase 1: materialize. Independent budget — sandbox cold-start
			// + checkpoint can run minutes on its own; if we shared a single
			// 10-min ctx with phases 2/3 a slow materialize would drain the
			// budget before download/apply/commit even start.
			materializeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			fmt.Fprintf(cmd.OutOrStdout(), "asking sandbox to checkpoint...\n")
			mres, err := dc.MaterializeMigration(materializeCtx, worktreeID)
			if err != nil {
				return fmt.Errorf("materialize: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "checkpoint %s (HEAD %s); downloading...\n", mres.CheckpointID, shortSHA(mres.HeadCommit))

			applyCtx, applyCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer applyCancel()

			// Phase 2: download + apply locally
			if err := applyRemoteCheckpoint(applyCtx, absRepo, mres); err != nil {
				return fmt.Errorf("apply checkpoint locally: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "applied to %s; committing ownership transfer...\n", absRepo)

			// Phase 3: commit ownership transfer
			res, err := dc.CommitMigration(applyCtx, worktreeID, mres.CheckpointID, mres.MigrationToken)
			if err != nil {
				return fmt.Errorf("commit migration (apply succeeded; rerun pull to retry the ownership flip): %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "reclaimed worktree %s → %s/%s\n", res.WorktreeID, res.NewOwnerKind, res.NewOwnerID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&alsoMig, "migrate", false, "download the sandbox's checkpoint and reclaim ownership")
	cmd.Flags().StringVar(&worktreeID, "worktree-id", "", "Worktree ID (default: read from <repo>/.clank/worktree-id)")
	return cmd
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
