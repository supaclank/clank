package clankcli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/clanksync/triggers"
)

// installTriggersFor installs the autopush triggers for the given
// harnesses (triggers.HarnessClaudeCode / HarnessOpenCode) that fire
// `clank push` on idle. Global + idempotent, so re-running across repos
// is harmless. Unknown harness names are skipped.
func installTriggersFor(cmd *cobra.Command, harnesses []string) error {
	var installed []string
	for _, h := range harnesses {
		switch h {
		case triggers.HarnessClaudeCode:
			if err := triggers.InstallClaude(); err != nil {
				return fmt.Errorf("install autopush triggers: %w", err)
			}
			installed = append(installed, "Claude Code hook")
		case triggers.HarnessOpenCode:
			if err := triggers.InstallOpenCode(); err != nil {
				return fmt.Errorf("install autopush triggers: %w", err)
			}
			installed = append(installed, "opencode plugin")
		}
	}
	if len(installed) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Installed autopush triggers (%s).\n", strings.Join(installed, " + "))
	}
	return nil
}
