package clankcli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/clanksync/triggers"
)

// installAutoPushTriggers installs the Claude hook + opencode plugin
// that fire `clank push` on idle. Global + idempotent, so re-running
// `clank init` in another repo is harmless.
func installAutoPushTriggers(cmd *cobra.Command) error {
	if err := triggers.Install(); err != nil {
		return fmt.Errorf("install autopush triggers: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Installed autopush triggers (Claude hook + opencode plugin).")
	return nil
}
