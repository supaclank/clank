package clankcli

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

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
