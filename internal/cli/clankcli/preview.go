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

	cmd := &cobra.Command{
		Use:   "preview [name | <folder> <url-or-:port>]",
		Short: "Preview an app with the Clank overlay",
		Long: `Make a project previewable, with a clank agent one gesture away.

Boots (or reuses) the local clank daemon, then launches or attaches to a preview:

  - Expo app: exposes the daemon to your phone over the LAN behind a
    one-time pairing token and prints a QR. Scan it with the clank app
    on the same Wi-Fi; shake to summon the prompt box.
  - Configured web app: starts the default entry in .clank/launch.yaml,
    or a named entry with clank preview web-app, then fronts the dev
    server with a local proxy that injects the clank overlay and opens
    your browser. If no config exists, Clank returns a one-time setup
    task for the connected agent. Expo keeps its automatic launch flow.
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

Everything is torn down on Ctrl+C — including the daemon, if clank preview
was the one that started it. An attached server is never stopped by Clank.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if tunnel {
				return fmt.Errorf("--tunnel isn't implemented yet; keep your phone and laptop on the same Wi-Fi for now")
			}
			attach, err := parsePreviewAttachArgs(args)
			if err != nil {
				return err
			}
			if attach != nil {
				if projectDir != "" {
					return fmt.Errorf("preview folder was provided both positionally and with --project")
				}
				return runAttachedPreview(attach.ProjectDir, attach.UpstreamURL, backend, port)
			}
			if err := rejectPathShapedArg(args); err != nil {
				return err
			}
			launchName, err := previewLaunchName(args)
			if err != nil {
				return err
			}
			return runPreview(projectDir, launchName, backend, port)
		},
	}

	cmd.Flags().StringVar(&projectDir, "project", "", "Project directory (default: current directory)")
	cmd.Flags().StringVar(&backend, "backend", "", "Backend to use: opencode (default), claude")
	cmd.Flags().IntVar(&port, "port", 0, "Browser-proxy port for web previews (default: auto-assigned). Phone previews use the daemon's bridge port.")
	cmd.Flags().BoolVar(&tunnel, "tunnel", false, "Expose over an encrypted tunnel for off-LAN phones (not yet implemented)")

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
