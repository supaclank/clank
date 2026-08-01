package clankcli

// `clank connect` — sign an agent backend in on this machine. Mirrors
// the `clank github connect` naming, and reuses the inbox's provider-auth
// flow verbatim (internal/tui.ConnectModel) rather than growing a second
// implementation of device flows, API-key prompts, and the claude
// setup-token relay.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
)

func connectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connect [backend]",
		Short: "Connect an agent backend (sign in a provider)",
		Long: `Signs a coding agent in on this machine so clank can run it.

With no argument, shows every backend with its connection state and asks
which one you want; with a backend name (opencode, claude, codex) it goes
straight to that backend's providers.

Credentials are stored by this machine's clank-host process — nothing is
sent to a gateway. Requires an interactive terminal.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			var backend agent.BackendType
			if len(args) == 1 {
				parsed, err := agent.ParseBackend(args[0])
				if err != nil {
					return err
				}
				backend = parsed
			}
			return runConnect(cmd.Context(), backend, os.Stdin, os.Stdout)
		},
	}
}

// connectHint is what non-interactive callers are told instead of being
// dropped into a TUI they can't drive.
const connectHint = "run `clank connect` in a terminal to sign an agent in"

// errConnectNeedsTTY is returned by every entry point that would open
// the connect UI without a terminal to render it into.
var errConnectNeedsTTY = fmt.Errorf("clank connect requires a terminal; %s", connectHint)
