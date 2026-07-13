package clankcli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func previewCmd() *cobra.Command {
	var projectDir string
	var backend string
	var port int
	var tunnel bool

	cmd := &cobra.Command{
		Use:   "preview [prompt]",
		Short: "Preview the current folder on your phone (Expo) or in your browser (Vite)",
		Long: `Make the current folder previewable, with a clank agent one gesture away.

Boots (or reuses) the local clank daemon and serves this folder's app:

  - Expo app: exposes the daemon to your phone over the LAN behind a
    one-time pairing token and prints a QR. Scan it with the clank app
    on the same Wi-Fi; shake to summon the prompt box.
  - Vite app (Svelte, React, …): fronts the dev server with a local
    proxy that injects the clank overlay, and opens your browser.
    Cmd/Ctrl+E summons the prompt box, holding Cmd/Ctrl points at
    elements to attach them as context, and tapping Caps Lock starts
    and stops dictation (with a clank-voice install or
    ` + "CLANK_VOICE_ASR_CMD" + `).

A prompt is optional: pass one to also start an agent on this folder and
watch it work in the preview. Pairing/proxy, the dev server, and the agent
are independent. Everything is torn down on Ctrl+C — including the daemon,
if clank preview was the one that started it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if tunnel {
				return fmt.Errorf("--tunnel isn't implemented yet; keep your phone and laptop on the same Wi-Fi for now")
			}
			return runPreview(projectDir, strings.Join(args, " "), backend, port)
		},
	}

	cmd.Flags().StringVar(&projectDir, "project", "", "Project directory (default: current directory)")
	cmd.Flags().StringVar(&backend, "backend", "", "Backend to use: opencode (default), claude")
	cmd.Flags().IntVar(&port, "port", 0, "Port to listen on — phone gateway (Expo) or browser proxy (web) (default: auto-assigned)")
	cmd.Flags().BoolVar(&tunnel, "tunnel", false, "Expose over an encrypted tunnel for off-LAN phones (not yet implemented)")

	return cmd
}
