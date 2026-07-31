package clankcli

import (
	"fmt"
	"io"

	"github.com/acksell/clank/internal/host/preview"
)

// printPackagerNote narrates this folder's install plan before the
// dev server starts, from the same Resolve the daemon's Detect uses —
// the CLI never re-derives config or manager precedence. Purely
// informational: nothing is asked and nothing is stored; previews use
// the project's own declared manager locally, and switching to bun is
// the project's own move (run `bun install` once; the resulting
// bun.lock flips detection). Resolution errors stay silent here —
// Preview.Start surfaces them through its canonical error path.
func printPackagerNote(projectDir string, out io.Writer) {
	res, err := preview.Resolve(projectDir, preview.PackagerPolicyReuseProject)
	if err != nil || res == nil {
		return
	}
	if res.InstallPinned {
		fmt.Fprintln(out, styleDim.Render("Installing per this repo's clank.yaml."))
		return
	}
	if preview.DependenciesPresent(res.EffectiveDir) {
		fmt.Fprintln(out, styleDim.Render("Using this folder's existing dependencies (installed by you; clank won't touch them)."))
	} else if res.DeclaredEvidence != "" || res.DeclaredPackager != preview.PackagerBun {
		fmt.Fprintf(out, "Installing with %s%s.\n", res.DeclaredPackager, evidenceSuffix(res.DeclaredEvidence))
	}
	if res.DeclaredPackager != preview.PackagerBun {
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
