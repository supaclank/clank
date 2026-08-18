package clankcli

// `clank connect` allows and configures agent harnesses on this machine. It
// reuses the inbox's provider-auth flow rather than growing a second
// implementation of device flows, API-key prompts, and the Claude setup-token
// relay.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/supaclank/clank/internal/agent"
)

func connectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connect [backend]",
		Short: "Allow and configure an agent harness",
		Long: `Allows Clank to control a coding-agent harness through ACP, then
detects or configures that harness's authentication.

With no argument, shows every harness and its allow/auth state. With a
harness name (opencode, claude, codex), it goes straight to that harness.
Credential detection starts only after you approve the harness.

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
const connectHint = "run `clank connect` in a terminal to allow and configure an agent harness"

// errConnectNeedsTTY is returned by every entry point that would open
// the connect UI without a terminal to render it into.
var errConnectNeedsTTY = fmt.Errorf("clank connect requires a terminal; %s", connectHint)
