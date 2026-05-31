package clankcli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// pullCmd registers `clank pull` — the asymmetric counterpart to
// `clank push`. It wakes the sandbox, checkpoints its current state
// (committed history + uncommitted changes + opencode sessions), and
// applies it into the local worktree.
func pullCmd() *cobra.Command {
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "pull [repo-path]",
		Short: "Pull the sandbox's current state into a local worktree",
		Long: `Wake the sandbox for this worktree, checkpoint its current state
(committed history + uncommitted changes + opencode sessions), and apply
it into the local worktree.

Pull refuses unless the local worktree is clean and fast-forwardable to
the sandbox tip — those preconditions are what make the restore safe, so
your local work is never clobbered. Waking the sandbox cancels any
sessions running there, so pull asks for confirmation first (skip with
--yes).`,
		Args: cobra.MaximumNArgs(1),
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
			isRepo, err := isGitRepo(absRepo)
			if err != nil {
				return fmt.Errorf("check git repo: %w", err)
			}
			if !isRepo {
				return fmt.Errorf("%s is not a git repository", absRepo)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()

			cli, err := activeRemoteSyncClient(ctx)
			if err != nil {
				return err
			}
			return runPull(ctx, cmd, cli, absRepo, assumeYes)
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt (pull cancels the sandbox's running sessions)")
	return cmd
}

// runPull is the testable core: clean-dir gate → resolve the tracked
// worktree id → confirm → wake+materialize the sandbox → apply locally.
// Takes an already-resolved sync client so tests can drive it against an
// httptest gateway without the active-remote/token machinery.
func runPull(ctx context.Context, cmd *cobra.Command, cli *syncclient.Client, absRepo string, assumeYes bool) error {
	// Clean-dir precondition — checked BEFORE waking the sandbox so a
	// dirty tree fails fast and cheap. The restore is a hard reset to the
	// sandbox state; uncommitted local work would be clobbered.
	clean, err := git.IsClean(absRepo)
	if err != nil {
		return fmt.Errorf("check working tree: %w", err)
	}
	if !clean {
		return fmt.Errorf("uncommitted changes in the working tree — `git stash`, then `clank pull`, then `git stash pop`")
	}

	worktreeID, err := agent.ReadLocalWorktreeID(absRepo)
	if err != nil {
		return fmt.Errorf("load cached worktree id: %w", err)
	}
	if worktreeID == "" {
		return fmt.Errorf("this worktree isn't tracked — nothing to pull (run `clank init`, then `clank push`, first)")
	}

	// Waking the sandbox cancels its in-flight sessions; gate the
	// destructive pull behind explicit confirmation.
	if !assumeYes {
		ok, err := confirmYesNo(cmd, "Pull wakes the sandbox and cancels any sessions running there. Continue? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
			return nil
		}
	}

	// Our current HEAD lets the gateway return only the head-chain slice
	// we lack. "" (e.g. an empty repo) ⇒ the full chain.
	localHEAD, err := git.HeadCommit(absRepo)
	if err != nil {
		localHEAD = ""
	}

	fmt.Fprintln(cmd.ErrOrStderr(), "Waking sandbox and checkpointing its current state…")
	res, err := cli.PullWorktree(ctx, worktreeID, localHEAD)
	if err != nil {
		return fmt.Errorf("materialize sandbox state: %w", err)
	}

	if err := applyRemotePull(ctx, http.DefaultClient, absRepo, res); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "%s pulled the sandbox's state into %s (checkpoint %s)\n",
		styleOK.Render("✓"), filepath.Base(absRepo), res.CheckpointID)
	return nil
}

// confirmYesNo prints prompt and reads one line from the command's
// input, returning true only for an explicit y/yes. EOF (piped input
// without a trailing newline) is parsed for whatever was read.
func confirmYesNo(cmd *cobra.Command, prompt string) (bool, error) {
	fmt.Fprint(cmd.OutOrStdout(), prompt)
	in := cmd.InOrStdin()
	if in == nil {
		in = os.Stdin
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
