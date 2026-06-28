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
		Short: "Preview your app on your phone (Expo-style) with a local clank agent",
		Long: `Boot a local, phone-reachable clank gateway and render a QR code.

Scan it with the clank app from a phone on the same Wi-Fi to open a live
preview of your app. Runtime errors flow back to the local agent, which
can fix them; the agent starts on the prompt you pass, so you watch it
work on your phone. Everything — the gateway, the dev server, and the
agent host — is torn down when you press Ctrl+C.`,
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
