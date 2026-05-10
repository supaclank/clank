package clankcli

import (
	cryptorand "crypto/rand"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/pkg/syncclient"
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
		baseURL   string
		token     string
		display   string
		deviceID  string
		repoPath  string
		alsoMig   bool
	)
	cmd := &cobra.Command{
		Use:   "push [repo-path]",
		Short: "Upload a checkpoint of a local worktree to clank-sync",
		Long: `Build a two-bundle checkpoint (HEAD history + uncommitted state)
of the repo at <repo-path> and upload it to clank-sync. The bundle
streams from the laptop directly to object storage via a presigned
URL — no bytes pass through clank-sync's process memory.

Worktree IDs are cached per-repo at <repo>/.clank/worktree-id; the
device identity is cached at ~/.config/clank/device-id and shared
across repos. First push registers the worktree and caches the ID.

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

			if baseURL == "" {
				return fmt.Errorf("--base-url is required (or set CLANK_SYNC_URL)")
			}
			if deviceID == "" {
				deviceID, err = ensureDeviceID()
				if err != nil {
					return fmt.Errorf("device id: %w", err)
				}
			}

			cli, err := syncclient.New(syncclient.Config{
				BaseURL:   baseURL,
				AuthToken: token,
				DeviceID:  deviceID,
			})
			if err != nil {
				return err
			}
			ctx := context.Background()

			worktreeID, err := agent.ReadLocalWorktreeID(absRepo)
			if err != nil {
				return fmt.Errorf("load cached worktree id: %w", err)
			}
			if worktreeID == "" {
				name := display
				if name == "" {
					name = filepath.Base(absRepo)
				}
				worktreeID, err = cli.RegisterWorktree(ctx, name)
				if err != nil {
					return fmt.Errorf("register worktree: %w", err)
				}
				if err := agent.WriteLocalWorktreeID(absRepo, worktreeID); err != nil {
					return fmt.Errorf("cache worktree id: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "registered worktree %s as %q\n", worktreeID, name)
			}

			res, err := cli.PushCheckpoint(ctx, worktreeID, absRepo)
			if err != nil {
				return fmt.Errorf("push checkpoint: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pushed checkpoint %s (HEAD %s)\n",
				res.CheckpointID, shortSHA(res.Manifest.HeadCommit))

			if alsoMig {
				dc, err := daemonclient.NewDefaultClient()
				if err != nil {
					return fmt.Errorf("daemon client: %w", err)
				}
				mctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				mres, err := dc.MigrateWorktree(mctx, worktreeID, deviceID, daemonclient.MigrateToRemote)
				if err != nil {
					return fmt.Errorf("migrate to remote: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "migrated worktree %s → %s/%s\n",
					mres.WorktreeID, mres.NewOwnerKind, mres.NewOwnerID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", envOrDefault("CLANK_SYNC_URL", ""), "clank-sync base URL")
	cmd.Flags().StringVar(&token, "token", envOrDefault("CLANK_SYNC_TOKEN", ""), "bearer token for clank-sync")
	cmd.Flags().StringVar(&display, "display-name", "", "display name for newly-registered worktrees (default: basename of repo-path)")
	cmd.Flags().StringVar(&deviceID, "device-id", envOrDefault("CLANK_DEVICE_ID", ""), "device id (default: ~/.config/clank/device-id, auto-generated on first run)")
	cmd.Flags().BoolVar(&alsoMig, "migrate", false, "after pushing, also transfer worktree ownership to the remote host")
	return cmd
}

// ensureDeviceID returns this laptop's stable device id, generating
// one on first call. Stored at ~/.config/clank/device-id.
func ensureDeviceID() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(configDir, "clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "device-id")
	if data, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		// A permission/IO error here would otherwise silently mint a
		// fresh device id and break ownership against existing
		// worktrees.
		return "", fmt.Errorf("read device id %s: %w", path, err)
	}
	buf := make([]byte, 16)
	if _, err := cryptorand.Read(buf); err != nil {
		return "", err
	}
	id := "dev-" + hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
