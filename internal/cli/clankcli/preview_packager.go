package clankcli

import (
	"fmt"
	"io"

	"github.com/acksell/clank/internal/host/preview"
)

// promptPackagerChoice runs the one-time-per-project packager
// question before the dev server starts (BEFORE Preview.Start — the
// prompt must never appear after minutes of install output). Only
// fires when the project's own signals point at a manager other than
// bun and no choice was saved yet; the answer persists in the clank
// state dir (never the repo) where the daemon's Detect reads it too.
//
// interactive gates the actual question (callers pass
// stdinIsTTY(in)); a non-interactive run just narrates the detection
// and leaves the choice unsaved so a later interactive run can ask.
// Detection errors stay silent here — Preview.Start surfaces them
// through its canonical error path.
func promptPackagerChoice(projectDir string, in io.Reader, out io.Writer, interactive bool) {
	if pm, ok := preview.LoadPackagerChoice(projectDir); ok {
		fmt.Fprintln(out, styleDim.Render(fmt.Sprintf("Installing with %s (your saved choice).", pm)))
		return
	}
	detected, evidence, err := preview.ResolvePackager(projectDir)
	if err != nil {
		return
	}
	if detected == preview.PackagerBun {
		// clank's own preference needs no question; narrate only when
		// a repo signal (bun.lock etc.) decided, to keep template
		// first-runs quiet.
		if evidence != "" {
			fmt.Fprintln(out, styleDim.Render(fmt.Sprintf("Installing with bun (%s).", evidence)))
		}
		return
	}

	fmt.Fprintf(out, "Detected %s (%s).\n", detected, evidence)
	fmt.Fprintln(out, styleCmdHint.Render(
		"Clank prefers bun for its ~10x faster installs and disk-space savings, letting you run more previews in parallel. "+
			"Most frameworks support bun nowadays, though pnpm workspaces and yarn PnP setups should keep their own manager. "+
			"We recommend switching, but it's up to you!"))
	if !interactive {
		fmt.Fprintln(out, styleDim.Render(fmt.Sprintf("Installing with %s.", detected)))
		return
	}

	fmt.Fprintf(out, "Use bun for clank's preview installs? [y/N] ")
	choice := detected
	if readYes(in) {
		choice = preview.PackagerBun
	}
	// Save either answer — the question is once per project, not once
	// per run. On failure, fall through to the detected manager and
	// say so; a broken state dir must not block the preview.
	if err := preview.SavePackagerChoice(projectDir, choice); err != nil {
		fmt.Fprintln(out, styleWarn.Render(fmt.Sprintf("Couldn't save the choice (%v) — using %s for this run.", err, detected)))
		return
	}
	fmt.Fprintln(out, styleDim.Render(fmt.Sprintf("Installing with %s (saved for this project).", choice)))
}
