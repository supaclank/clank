package host

// The machine's own claude CLI login as a status signal. When clank's
// anthropic sink is empty, AnthropicEnv returns nil and the spawned
// claude resolves its own credential — the macOS Keychain item
// "Claude Code-credentials" or ~/.claude/.credentials.json. This file
// answers "would that resolution find a login?" by presence checks
// only: no token bytes are ever read, decoded, or copied.

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// claudeKeychainService is the Keychain service name the claude CLI
// stores its OAuth credentials under on macOS.
const claudeKeychainService = "Claude Code-credentials"

// claudeKeychainProbeTimeout bounds the `security` probe. It reads
// local keychain state — anything slower is a hung helper, not a slow
// network.
const claudeKeychainProbeTimeout = 3 * time.Second

// lookPath resolves the security binary; a package var so tests can
// simulate its absence without mutating the process-wide PATH (which
// would race with unrelated t.Parallel() tests that spawn git).
var lookPath = exec.LookPath

// claudeCLILoginPresent reports whether the machine's claude CLI has a
// stored login the spawned claude would pick up on its own. False on
// any probe failure — for the status fallback that's an ordinary "no
// credential here", not a fault to surface.
func claudeCLILoginPresent(ctx context.Context, homeDir string) bool {
	if claudeCredentialsFileExists(homeDir) {
		return true
	}
	return claudeKeychainEntryExists(ctx)
}

// claudeCredentialsFileExists checks the claude CLI's file-based
// credential store (Linux, and macOS setups predating Keychain use).
// Stat-only: presence and non-emptiness, never the contents.
func claudeCredentialsFileExists(homeDir string) bool {
	fi, err := os.Stat(filepath.Join(homeDir, ".claude", ".credentials.json"))
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

// claudeKeychainEntryExists probes the macOS Keychain for the claude
// CLI's credential item. Exit code only — without -w, `security` does
// a metadata search: no secret bytes are read. A locked keychain is
// checked first via show-keychain-info, since find-generic-password on
// a locked keychain triggers a disruptive GUI unlock prompt.
// On platforms without `security` the probe is skipped.
func claudeKeychainEntryExists(ctx context.Context) bool {
	path, err := lookPath("security")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, claudeKeychainProbeTimeout)
	defer cancel()

	if exec.CommandContext(ctx, path, "show-keychain-info").Run() != nil {
		return false // locked (or unreadable) — avoid the unlock prompt
	}

	cmd := exec.CommandContext(ctx, path, "find-generic-password", "-s", claudeKeychainService)
	// Attribute output includes the account label (often an email) —
	// discard it; only found/not-found matters.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}
