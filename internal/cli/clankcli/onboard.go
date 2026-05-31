package clankcli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/acksell/clank/internal/clanksync/triggers"
	"github.com/acksell/clank/internal/config"
)

// isInteractive reports whether clank may prompt the user: both stdin and
// stdout must be terminals. Autopush triggers (the Claude Code Stop hook,
// the opencode plugin) run non-interactively, so this gates EVERY prompt
// — a prompt that blocked on stdin there would hang the agent's turn.
//
// When in/out are not *os.File (e.g. a test buffer, or a pipe), this is
// false, which keeps the safe non-interactive error path.
func isInteractive(cmd *cobra.Command) bool {
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return false
	}
	out, ok := cmd.OutOrStdout().(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

// readLine reads a single line from the command's input WITHOUT buffering
// past the newline, so consecutive prompts sharing one reader (e.g. piped
// test input "y\n2\n") don't lose bytes to a discarded bufio buffer. The
// trailing newline and any CR are stripped.
func readLine(cmd *cobra.Command) (string, error) {
	in := cmd.InOrStdin()
	if in == nil {
		in = os.Stdin
	}
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("read input: %w", err)
		}
	}
	return strings.TrimRight(sb.String(), "\r"), nil
}

// allHarnesses is the safe default when the user hasn't chosen: install
// triggers for every supported harness.
func allHarnesses() []string {
	return []string{triggers.HarnessClaudeCode, triggers.HarnessOpenCode}
}

// pickHarnesses asks which coding-agent harnesses to auto-sync sessions
// for. A bare Enter (or an unrecognized answer) selects both. The Claude
// option is the Claude Code CLI / Agent SDK Stop hook — it covers any app
// built on the Claude CLI/SDK, but NOT Claude Desktop (untrackable).
func pickHarnesses(cmd *cobra.Command) ([]string, error) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Which coding agents should clank auto-sync sessions for?")
	fmt.Fprintln(out, "  1) Claude Code (CLI / Agent SDK)")
	fmt.Fprintln(out, "  2) opencode")
	fmt.Fprintln(out, "  3) Both")
	fmt.Fprint(out, "Select [3]: ")
	line, err := readLine(cmd)
	if err != nil {
		return nil, err
	}
	switch strings.TrimSpace(line) {
	case "1":
		return []string{triggers.HarnessClaudeCode}, nil
	case "2":
		return []string{triggers.HarnessOpenCode}, nil
	default:
		return allHarnesses(), nil
	}
}

// ensureHarnessTriggers resolves which harnesses to install autopush
// triggers for and installs them. Resolution order:
//   - already chosen (prefs.SyncHarnesses) → install those (idempotent).
//   - interactive + unchosen → prompt, persist the choice, install.
//   - non-interactive + unchosen → install both (safe default), no persist.
func ensureHarnessTriggers(cmd *cobra.Command, interactive bool) error {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return fmt.Errorf("load preferences: %w", err)
	}
	if len(prefs.SyncHarnesses) > 0 {
		return installTriggersFor(cmd, prefs.SyncHarnesses)
	}
	if !interactive {
		return installTriggersFor(cmd, allHarnesses())
	}
	chosen, err := pickHarnesses(cmd)
	if err != nil {
		return err
	}
	if err := config.UpdatePreferences(func(p *config.Preferences) {
		p.SyncHarnesses = chosen
	}); err != nil {
		return fmt.Errorf("save harness selection: %w", err)
	}
	return installTriggersFor(cmd, chosen)
}
