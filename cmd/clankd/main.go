// clankd is the Clank daemon manager. It provides direct access to
// daemon lifecycle commands: start, stop, and status.
package main

import (
	"os"

	"github.com/acksell/clank/internal/cli/daemoncli"
)

func main() {
	cmd := daemoncli.Command()
	cmd.Use = "clankd"
	cmd.Short = "Clank daemon manager"
	cmd.Long = "clankd manages the Clank background daemon that runs coding agent sessions.\n\nThis is equivalent to 'clank daemon'."
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
