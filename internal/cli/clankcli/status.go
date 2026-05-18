package clankcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// statusCmd registers `clank status` — a git-style summary of the
// current repo's worktree and its ownership on the active remote.
// Hits the active remote directly via NewRemoteClient (same path as
// `clank push --migrate`), so it works even when the local clankd
// isn't running.
func statusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status [repo-path]",
		Short: "Show the current worktree's local/remote status",
		Long: `Print a concise summary of this repo's worktree and where its
ownership currently lives. Without arguments, uses the current directory.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
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
			abs, err := filepath.Abs(repoPath)
			if err != nil {
				return fmt.Errorf("resolve repo path: %w", err)
			}
			out, err := runStatus(cmd.Context(), abs)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
	return cmd
}

// statusReport is the data view rendered by `clank status`. Kept as a
// flat struct so tests can build it directly without I/O.
type statusReport struct {
	WorktreeID         string                       // empty when no .clank/worktree-id is cached
	ActiveRemote       string                       // remote profile name; "" when none configured
	ActiveRemoteURL    string                       // for display
	RemoteError        error                        // populated when ListWorktrees failed
	WorktreeFromRemote *daemonclient.WorktreeInfo   // nil when not found / no remote
}

// runStatus assembles the report by reading .clank/worktree-id and
// querying the active remote (if configured). Rendering lives in
// renderStatusReport so tests can hit it without a running remote.
func runStatus(ctx context.Context, repoPath string) (string, error) {
	wtID, err := agent.ReadLocalWorktreeID(repoPath)
	if err != nil {
		return "", fmt.Errorf("read cached worktree id: %w", err)
	}

	rep := statusReport{WorktreeID: wtID}

	prefs, err := config.LoadPreferences()
	if err != nil {
		return "", fmt.Errorf("load preferences: %w", err)
	}
	if p := prefs.ActiveRemote(); p != nil {
		rep.ActiveRemote = prefs.Remote.Active
		rep.ActiveRemoteURL = p.GatewayURL
	}

	if rep.ActiveRemote != "" && wtID != "" {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cli, err := daemonclient.NewRemoteClient()
		if err == nil {
			wts, err := cli.ListWorktrees(ctx)
			if err != nil {
				rep.RemoteError = err
			} else {
				for i := range wts {
					if wts[i].ID == wtID {
						rep.WorktreeFromRemote = &wts[i]
						break
					}
				}
			}
		} else {
			rep.RemoteError = err
		}
	}

	return renderStatusReport(rep), nil
}

// (Styles moved to style.go so push/pull/status share the same palette.)

// renderStatusReport produces a git-status-flavoured paragraph:
// one-line headline + optional bullet under it. Branches by scenario
// rather than always-show-every-field.
func renderStatusReport(rep statusReport) string {
	var sb strings.Builder

	switch {
	case rep.WorktreeID == "":
		// Not registered yet.
		sb.WriteString(styleWarn.Render("Not synced") + " — this repo has no worktree id.\n")
		if rep.ActiveRemote == "" {
			sb.WriteString("  " + styleDim.Render("Run `clank remote add`, then `clank push` to register.") + "\n")
		} else {
			sb.WriteString("  " + styleDim.Render(fmt.Sprintf("Run `clank push` to register and sync to the %s remote.", rep.ActiveRemote)) + "\n")
		}
		return sb.String()
	}

	// Headline always names the worktree.
	sb.WriteString("On worktree ")
	sb.WriteString(styleWorktree.Render(rep.WorktreeID))
	sb.WriteString("\n")

	switch {
	case rep.ActiveRemote == "":
		sb.WriteString("  " + styleLocalOwner.Render("Owned by this laptop") + " " + styleDim.Render("(no remote configured)") + "\n")

	case rep.RemoteError != nil:
		sb.WriteString("  Owner " + styleErr.Render("unknown") + " — " + styleRemoteOwner.Render(rep.ActiveRemote) + " remote unreachable: " + styleDim.Render(rep.RemoteError.Error()) + "\n")

	case rep.WorktreeFromRemote == nil:
		sb.WriteString("  " + styleWarn.Render("Not on") + " " + styleRemoteOwner.Render(rep.ActiveRemote) + " remote " + styleDim.Render("(was it removed?)") + "\n")
		sb.WriteString("  " + styleDim.Render("Run `clank push` to re-register this worktree.") + "\n")

	case rep.WorktreeFromRemote.OwnerKind == "remote":
		sb.WriteString("  Owned by " + styleRemoteOwner.Render(rep.ActiveRemote) + " remote " + styleDim.Render("("+rep.ActiveRemoteURL+")") + "\n")
		sb.WriteString("  " + renderLatest(rep.WorktreeFromRemote.LatestSyncedCheckpoint) + "\n")

	default: // local-owned, remote knows about it
		sb.WriteString("  " + styleLocalOwner.Render("Owned by this laptop") + "\n")
		if cp := rep.WorktreeFromRemote.LatestSyncedCheckpoint; cp != "" {
			sb.WriteString("  " + styleDim.Render("Synced to "+rep.ActiveRemote+" remote — latest checkpoint "+cp) + "\n")
		} else {
			sb.WriteString("  " + styleDim.Render("Not yet pushed to "+rep.ActiveRemote+" remote") + "\n")
		}
	}

	return sb.String()
}

// renderLatest formats the "Latest sync: …" bullet for a remote-owned
// worktree. Falls back to a dim "no checkpoint yet" line when empty
// (rare: remote-owned implies a push happened, but a corrupt row
// could leave the field empty).
func renderLatest(checkpointID string) string {
	if checkpointID == "" {
		return styleDim.Render("No synced checkpoint recorded yet")
	}
	return styleDim.Render("Latest checkpoint: " + checkpointID)
}
