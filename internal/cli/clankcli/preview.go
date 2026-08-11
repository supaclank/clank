package clankcli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supaclank/clank/internal/sharetunnel"
)

func previewCmd() *cobra.Command {
	var projectDir string
	var backend string
	var port int
	var share bool

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

--share additionally publishes web previews on a public Cloudflare
quick-tunnel URL (requires the cloudflared binary) for view-only
feedback: the link serves the plain app straight from the dev server,
while the clank overlay, agent, and daemon stay on this machine's
private preview URL. Anyone with the link can browse the dev server
until the preview stops.

Everything is torn down on Ctrl+C — including the daemon, if clank preview
was the one that started it. An attached server is never stopped by Clank.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if share {
				// Fail before any daemon or dev-server work when the
				// tunnel client is missing.
				if _, err := sharetunnel.FindBinary(); err != nil {
					return err
				}
			}
			if len(args) == 1 && isWebURLArg(args[0]) {
				if projectDir != "" {
					return fmt.Errorf("--project cannot be used with a GitHub pull request URL")
				}
				locator, err := parseGitHubPullRequestURL(args[0])
				if err != nil {
					return err
				}
				return runGitHubPullRequestPreview(locator, "", backend, port, share, cmd.InOrStdin(), cmd.OutOrStdout())
			}
			attach, err := parsePreviewAttachArgs(args)
			if err != nil {
				return err
			}
			if attach != nil {
				if projectDir != "" {
					return fmt.Errorf("preview folder was provided both positionally and with --project")
				}
				return runAttachedPreview(attach.ProjectDir, attach.UpstreamURL, backend, port, share)
			}
			if err := rejectPathShapedArg(args); err != nil {
				return err
			}
			launchName, err := previewLaunchName(args)
			if err != nil {
				return err
			}
			return runPreview(projectDir, launchName, backend, port, share)
		},
	}

	cmd.Flags().StringVar(&projectDir, "project", "", "Project directory (default: current directory)")
	cmd.Flags().StringVar(&backend, "backend", "", "Backend to use: opencode (default), claude")
	cmd.Flags().IntVar(&port, "port", 0, "Browser-proxy port for web previews (default: auto-assigned). Phone previews use the daemon's bridge port.")
	cmd.Flags().BoolVar(&share, "share", false, "Also publish the app on a public view-only tunnel URL (Cloudflare quick tunnel; requires cloudflared). Web previews only.")

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
