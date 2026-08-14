package clankcli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func previewCmd() *cobra.Command {
	var projectDir string
	var backend string
	var port int
	var tunnel bool
	var attachSession string

	cmd := &cobra.Command{
		Use:   "preview [name | <github-pr-url> | <folder> <url-or-:port>]",
		Short: "Preview an app with the Clank overlay",
		Long: `Make a project previewable, with a clank agent one gesture away.

Boots (or reuses) the local clank daemon, then launches or attaches to a preview:

  - GitHub pull request URL: resolves the PR without downloading it, shows
    its author and exact commit SHA for approval, then checks that revision
    out on its real branch for same-repository PRs and starts its configured
    preview. An existing checkout is reused; otherwise Clank creates a managed
    worktree. Fork PRs use an isolated exact-revision branch. Public repos work
    anonymously; private repos require GitHub Connect.
  - Expo app: exposes the daemon to your phone over the LAN behind a
    one-time pairing token and prints a QR. Scan it with the clank app
    on the same Wi-Fi; shake to summon the prompt box.
  - Configured web app: starts the default entry in .clank/launch.yaml,
    or a named entry with clank preview web-app, then fronts the dev
    server with a local proxy that injects the clank overlay and opens
    your browser. If no config exists, Clank offers a one-time setup and
    runs the connected agent inline to generate it. Expo keeps its
    automatic launch flow.
    Cmd/Ctrl+E summons the prompt box, holding Cmd/Ctrl points at
    elements to attach them as context, and tapping Caps Lock starts
    and stops dictation. On first dictation you pick the engine —
    fully local (clank-voice running the ~670 MB NVIDIA Parakeet v3
    model on your machine, or ` + "CLANK_VOICE_ASR_CMD" + `) or the
    browser's Web Speech API (audio goes to the browser vendor's
    speech service) — and can switch later via the chevron next to
    the mic. Highlight text or attach an element to pin an inline
    comment on it; several comments ride one submit.
  - Existing web server (clank preview . :5173): fronts a server the
    user already started. The folder and URL may appear in either order.
    A :port target expands to http://127.0.0.1:port. Clank never starts
    or stops the attached server; the folder supplies project context
    to the overlay and agent.
  - Existing agent session (--attach): binds the overlay to a session
    that already exists instead of creating one on the first prompt.
    Bare --attach opens a picker sorted by last activity with an in-list
    rediscover action for sessions clank hasn't registered yet;
    --attach=<session-id> (clank or backend-external id) attaches
    directly, rediscovering the project's sessions if the id is unknown.
    Web previews only.

Everything is torn down on Ctrl+C — including the daemon, if clank preview
was the one that started it. An attached server is never stopped by Clank.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if tunnel {
				return fmt.Errorf("--tunnel isn't implemented yet; keep your phone and laptop on the same Wi-Fi for now")
			}
			if attachSession == previewAttachSelect && len(args) == 1 && looksLikeSessionID(args[0]) {
				return fmt.Errorf("%s looks like a session id — write it as --attach=%s (a space-separated value is read as a launch name)", args[0], args[0])
			}
			if len(args) == 1 && isWebURLArg(args[0]) {
				if attachSession != "" {
					return fmt.Errorf("--attach cannot be used with a GitHub pull request URL — its preview always drives a fresh session")
				}
				if projectDir != "" {
					return fmt.Errorf("--project cannot be used with a GitHub pull request URL")
				}
				locator, err := parseGitHubPullRequestURL(args[0])
				if err != nil {
					return err
				}
				return runGitHubPullRequestPreview(locator, "", backend, port, cmd.InOrStdin(), cmd.OutOrStdout())
			}
			attach, err := parsePreviewAttachArgs(args)
			if err != nil {
				return err
			}
			if attach != nil {
				if projectDir != "" {
					return fmt.Errorf("preview folder was provided both positionally and with --project")
				}
				return runAttachedPreview(attach.ProjectDir, attach.UpstreamURL, backend, port, attachSession)
			}
			if err := rejectPathShapedArg(args); err != nil {
				return err
			}
			launchName, err := previewLaunchName(args)
			if err != nil {
				return err
			}
			return runPreview(projectDir, launchName, backend, port, attachSession)
		},
	}

	cmd.Flags().StringVar(&projectDir, "project", "", "Project directory (default: current directory)")
	cmd.Flags().StringVar(&backend, "backend", "", "Backend to use: opencode (default), claude")
	cmd.Flags().IntVar(&port, "port", 0, "Browser-proxy port for web previews (default: auto-assigned). Phone previews use the daemon's bridge port.")
	cmd.Flags().BoolVar(&tunnel, "tunnel", false, "Expose over an encrypted tunnel for off-LAN phones (not yet implemented)")
	cmd.Flags().StringVar(&attachSession, "attach", "", "Attach the overlay to an existing agent session: bare --attach picks from a list sorted by last activity; --attach=<session-id> (clank or backend-external id) attaches directly")
	cmd.Flags().Lookup("attach").NoOptDefVal = previewAttachSelect

	return cmd
}

// rejectPathShapedArg errors out on a single path-shaped argument
// (contains a dot or separator) rather than letting it silently start
// an agent — `clank preview <file>` opened that file until this
// command dropped file mode. This runs before the generic positional-argument
// error so old file-preview invocations receive actionable guidance.
func rejectPathShapedArg(args []string) error {
	if len(args) != 1 {
		return nil
	}
	arg := args[0]
	if !strings.ContainsAny(arg, "./"+string(os.PathSeparator)) {
		return nil
	}
	return fmt.Errorf("%s looks like a file path — clank preview <file> was removed; run clank preview with no arguments to preview the project", arg)
}
