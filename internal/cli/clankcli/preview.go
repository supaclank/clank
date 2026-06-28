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
		Short: "Preview the current folder on your phone (Expo-style)",
		Long: `Make the current folder previewable on your phone, Expo-style.

Boots (or reuses) the local clank daemon, exposes it to your phone over the
LAN behind a one-time pairing token, and serves this folder's Expo app.
Scan the QR with the clank app on the same Wi-Fi to open it.

A prompt is optional: pass one to also start an agent on this folder and
watch it work on your phone. Pairing, the dev server, and the agent are
independent. Everything is torn down on Ctrl+C — including the daemon, if
clank preview was the one that started it.`,
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
	cmd.Flags().IntVar(&port, "port", 0, "Gateway port to listen on (default: auto-assigned)")
	cmd.Flags().BoolVar(&tunnel, "tunnel", false, "Expose over an encrypted tunnel for off-LAN phones (not yet implemented)")

	return cmd
}
