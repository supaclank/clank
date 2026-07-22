package host

import (
	"context"
	"os"
	"os/exec"
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

	if !claudeCLILoginPresent(context.Background(), home) {
		t.Error("want present=true with a non-empty ~/.claude/.credentials.json")
	}
}

func TestClaudeCLILoginPresent_KeychainEntry(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `if [ "$1" = "find-generic-password" ] || [ "$1" = "show-keychain-info" ]; then exit 0; fi; exit 1`)

	// Empty home: only the keychain probe can answer.
	if !claudeCLILoginPresent(context.Background(), t.TempDir()) {
		t.Error("want present=true when the keychain probe finds the entry")
	}
}

func TestClaudeCLILoginPresent_NoLoginAnywhere(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `exit 44`)

	if claudeCLILoginPresent(context.Background(), t.TempDir()) {
		t.Error("want present=false with no file and no keychain entry")
	}
}

func TestClaudeCLILoginPresent_EmptyCredentialsFile(t *testing.T) {
	// Not parallel: t.Setenv (PATH).
	putFakeSecurity(t, `exit 44`)
	home := t.TempDir()
	writeClaudeCredentialsFile(t, home, "")

	if claudeCLILoginPresent(context.Background(), home) {
		t.Error("want present=false for a zero-byte credentials file")
	}
}

func TestClaudeKeychainEntryExists_NoSecurityBinary(t *testing.T) {
	t.Parallel()
	// Override the lookPath seam instead of mutating PATH — this
	// package has unrelated t.Parallel() tests that spawn git, and
	// clobbering process-wide PATH would race with them.
	orig := lookPath
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookPath = orig })

	if claudeKeychainEntryExists(context.Background()) {
		t.Error("want present=false when no security binary exists")
	}
}
