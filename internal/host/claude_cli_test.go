package host

import (
	"os"
	"path/filepath"
	"testing"
)

// putFakeSecurity installs an executable `security` stub at the FRONT
// of PATH so exec.LookPath resolves it ahead of the real macOS binary —
// which is also what keeps these tests hermetic on a dev laptop with a
// logged-in claude CLI in the real Keychain.
func putFakeSecurity(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "security"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeClaudeCredentialsFile seeds homeDir with a claude CLI
// credentials file of the given content.
func writeClaudeCredentialsFile(t *testing.T, homeDir, content string) {
	t.Helper()
	dir := filepath.Join(homeDir, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCLILoginPresent_CredentialsFile(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	// Keychain says no — the file alone must carry the answer.
	putFakeSecurity(t, `exit 44`) // errSecItemNotFound
	home := t.TempDir()
	writeClaudeCredentialsFile(t, home, `{"claudeAiOauth":{}}`)

	if !claudeCLILoginPresent(home) {
		t.Error("want present=true with a non-empty ~/.claude/.credentials.json")
	}
}

func TestClaudeCLILoginPresent_KeychainEntry(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `[ "$1" = "find-generic-password" ] && exit 0; exit 1`)

	// Empty home: only the keychain probe can answer.
	if !claudeCLILoginPresent(t.TempDir()) {
		t.Error("want present=true when the keychain probe finds the entry")
	}
}

func TestClaudeCLILoginPresent_NoLoginAnywhere(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `exit 44`)

	if claudeCLILoginPresent(t.TempDir()) {
		t.Error("want present=false with no file and no keychain entry")
	}
}

func TestClaudeCLILoginPresent_EmptyCredentialsFile(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `exit 44`)
	home := t.TempDir()
	writeClaudeCredentialsFile(t, home, "")

	if claudeCLILoginPresent(home) {
		t.Error("want present=false for a zero-byte credentials file")
	}
}

func TestClaudeKeychainEntryExists_NoSecurityBinary(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	// PATH with only an empty dir: LookPath("security") must miss, and
	// the probe must degrade to false instead of erroring (the Linux
	// laptop case).
	t.Setenv("PATH", t.TempDir())

	if claudeKeychainEntryExists() {
		t.Error("want present=false when no security binary exists")
	}
}
