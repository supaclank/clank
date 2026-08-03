package clankcli

// `clank github` subcommand — wraps the GitHub Connect daemon-client
// methods in a thin CLI. Three operations:
//
//   clank github connect      — start the device flow, open the
//                                verification URL in the user's
//                                browser, poll until success/error.
//   clank github status       — show whether GitHub is connected.
//   clank github disconnect   — clear the host's stored credential.
//   clank github pr           — push the current worktree's branch
//                                and open a PR. Flags supply title,
//                                body, base, draft.
//
// Future work: replace `connect` and `pr` with a Bubble Tea modal
// for richer UX. The CLI surface ships first so laptop users get an
// actionable hook without waiting on the modal polish.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/supaclank/clank/internal/daemonclient"
)

func githubCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github",
		Short: "Manage GitHub Connect (push branches + open PRs)",
		Long: `GitHub Connect lets Clank push your worktree branches and open pull
requests on your behalf. Credentials live on this host's clank-host
process — no shared bot account, no gateway-side storage.`,
	}
	cmd.AddCommand(
		githubConnectCmd(),
		githubStatusCmd(),
		githubDisconnectCmd(),
		githubPRCmd(),
	)
	return cmd
}

func githubStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show GitHub connection status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			client, err := ensureDaemon()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			st, err := client.GitHubStatus(ctx)
			if err != nil {
				return err
			}
			if !st.Available {
				fmt.Println("GitHub Connect is not enabled on this host (CLANK_GITHUB_OAUTH_CLIENT_ID unset).")
				return nil
			}
			if !st.Connected {
				fmt.Println("Not connected. Run `clank github connect` to sign in.")
				return nil
			}
			fmt.Printf("Connected as @%s.\n", st.GitHubLogin)
			if len(st.Scopes) > 0 {
				fmt.Printf("Scopes: %v\n", st.Scopes)
			}
			if !st.InstalledAt.IsZero() {
				fmt.Printf("Connected at: %s\n", st.InstalledAt.Local().Format(time.RFC3339))
			}
			return nil
		},
	}
}

func githubDisconnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect",
		Short: "Clear stored GitHub credentials",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			client, err := ensureDaemon()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.GitHubDisconnect(ctx); err != nil {
				return err
			}
			fmt.Println("Disconnected.")
			return nil
		},
	}
}

func githubConnectCmd() *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect GitHub via device flow",
		Long: `Starts the GitHub OAuth device flow. Prints a verification URL +
short code, opens the URL in your default browser, and polls until you
authorize or the flow is denied/expires.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			client, err := ensureDaemon()
			if err != nil {
				return err
			}

			// The connect flow can take a minute or two while the user
			// authorizes in the browser. Use a generous overall timeout
			// (device codes typically live 15 minutes).
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			start, err := client.GitHubConnectStart(ctx)
			if err != nil {
				return fmt.Errorf("start flow: %w", err)
			}
			fmt.Printf("\nOpen this URL to authorize:\n  %s\n\nCode: %s\n\n",
				start.VerificationURIComplete, start.UserCode)
			if !noOpen {
				_ = openBrowser(start.VerificationURIComplete)
			}
			fmt.Println("Waiting for you to authorize…")

			interval := time.Duration(start.Interval) * time.Second
			if interval < time.Second {
				interval = 5 * time.Second
			}
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(interval):
				}
				st, err := client.GitHubConnectStatus(ctx, start.FlowID)
				if err != nil {
					if errors.Is(err, daemonclient.ErrGitHubUnknownFlow) {
						return fmt.Errorf("flow expired before completion")
					}
					return fmt.Errorf("poll status: %w", err)
				}
				switch st.State {
				case daemonclient.GitHubFlowSuccess:
					if st.GitHubLogin != "" {
						fmt.Printf("Connected as @%s.\n", st.GitHubLogin)
					} else {
						fmt.Println("Connected.")
					}
					return nil
				case daemonclient.GitHubFlowDenied:
					return fmt.Errorf("authorization denied")
				case daemonclient.GitHubFlowExpired:
					return fmt.Errorf("device code expired")
				case daemonclient.GitHubFlowError:
					return fmt.Errorf("flow failed: %s", st.Error)
				case daemonclient.GitHubFlowCanceled:
					return fmt.Errorf("flow canceled")
				case daemonclient.GitHubFlowPending:
					// keep polling
				}
			}
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Don't auto-open the browser; print the URL only.")
	return cmd
}

func githubPRCmd() *cobra.Command {
	var (
		worktreeID string
		title      string
		body       string
		base       string
		draft      bool
	)
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Open a pull request for a worktree's branch",
		Long: `Pushes the worktree's current branch and opens a pull request
against --base. The worktree is identified by --worktree (a worktree
id). Title and base are required; body is optional.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if worktreeID == "" || title == "" || base == "" {
				return fmt.Errorf("--worktree, --title, and --base are required")
			}
			client, err := ensureDaemon()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			out, err := client.GitHubCreatePR(ctx, worktreeID, daemonclient.GitHubCreatePRRequest{
				Title: title,
				Body:  body,
				Base:  base,
				Draft: draft,
			})
			if err != nil {
				var conflict *daemonclient.GitHubPRAlreadyExistsError
				if errors.As(err, &conflict) {
					fmt.Printf("A pull request for this branch already exists.\n")
					if conflict.ExistingURL != "" {
						fmt.Printf("Existing PR: %s\n", conflict.ExistingURL)
					}
					return nil
				}
				return err
			}
			fmt.Printf("PR #%d opened: %s\n", out.PRNumber, out.PRURL)
			fmt.Printf("  %s → %s (head %s)\n", out.HeadBranch, out.BaseBranch, shortHeadSHA(out.HeadSHA))
			return nil
		},
	}
	cmd.Flags().StringVar(&worktreeID, "worktree", "", "Worktree id (required)")
	cmd.Flags().StringVar(&title, "title", "", "PR title (required)")
	cmd.Flags().StringVar(&body, "body", "", "PR body (optional)")
	cmd.Flags().StringVar(&base, "base", "", "Base branch on origin (required)")
	cmd.Flags().BoolVar(&draft, "draft", false, "Open as a draft PR")
	return cmd
}

// openBrowser tries to open url in the user's default browser. Best-
// effort: failure is logged and the caller falls back to the printed
// URL. Mirrors what `gh` does internally for consistency.
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "linux":
		cmd = exec.Command("xdg-open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	return cmd.Start()
}

// shortHeadSHA truncates a 40-char hex to 7 chars, the convention used by
// git log --oneline and GitHub URLs.
func shortHeadSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
