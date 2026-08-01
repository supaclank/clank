package clankcli

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// isInteractiveTerminal reports whether both ends of a full-screen
// prompt are real terminals — a TUI needs to read keys AND draw. Piped
// or redirected invocations answer false so callers can fail cleanly
// instead of hanging on a UI nobody can drive.
func isInteractiveTerminal(in io.Reader, out io.Writer) bool {
	return fileIsTTY(in) && fileIsTTY(out)
}

// fileIsTTY reports whether v is an *os.File attached to a terminal.
// term.IsTerminal, not os.ModeCharDevice — /dev/null is a char device
// too and is the usual non-interactive stand-in.
func fileIsTTY(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// clearTerminal wipes the visible screen AND (best-effort, ESC[3J)
// the scrollback, then homes the cursor. Used to retire a displayed
// pairing QR the moment a phone connects — when the QR carried the
// secret, leaving it in scrollback is a credential in history. No-op
// when stdout isn't a terminal (term.IsTerminal, not ModeCharDevice —
// /dev/null is a char device too).
func clearTerminal() {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	fmt.Print("\x1b[2J\x1b[3J\x1b[H")
}
