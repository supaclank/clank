package clankcli

import (
	"fmt"
	"io"

	"github.com/acksell/clank/internal/host/preview"
)

// printPackagerNote narrates this folder's install plan before the
// dev server starts. Purely informational — nothing is asked and
// nothing is stored: previews always use the project's own declared
// manager locally, and switching to bun is the project's own move
// (run `bun install` once; the resulting bun.lock flips detection).
// The bun tip rides along whenever a slower manager is declared.
// Detection errors stay silent here — Preview.Start surfaces them
// through its canonical error path.
func printPackagerNote(projectDir string, out io.Writer) {
	detected, evidence, err := preview.ResolvePackager(projectDir)
	if err != nil {
		return
	}
	if preview.DependenciesPresent(projectDir) {
		fmt.Fprintln(out, styleDim.Render("Using this folder's existing dependencies (installed by you; clank won't touch them)."))
	} else if evidence != "" || detected != preview.PackagerBun {
		fmt.Fprintf(out, "Installing with %s%s.\n", detected, evidenceSuffix(evidence))
	}
	if detected != preview.PackagerBun {
		fmt.Fprintln(out, styleCmdHint.Render(
			"Tip: bun installs are ~10x faster and much lighter on disk, so more previews can run in parallel. "+
				"To switch this project, run `bun install` once — clank follows the bun.lock it creates."))
	}
}

func evidenceSuffix(evidence string) string {
	if evidence == "" {
		return ""
	}
	return " (" + evidence + ")"
}
