package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// PinnedCodexVersion is the codex CLI version clank ships against, and the
// version of github.com/pmenglund/codex-sdk-go pinned in go.mod — the SDK's
// generated protocol types are only guaranteed to match this binary version.
// Bump both together (SDK tags mirror upstream codex releases).
const PinnedCodexVersion = "0.144.6"

// codexBinaryName is the executable looked up on PATH when a backend has no
// explicit CodexPath override.
const codexBinaryName = "codex"

// CodexVersion probes the installed codex CLI version, e.g. "0.144.6".
// Returns an error when the binary is missing or its output is unparseable.
func CodexVersion(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, codexBinaryName, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("probe codex version: %w", err)
	}
	v := parseCodexVersionOutput(string(out))
	if v == "" {
		return "", fmt.Errorf("unexpected `codex --version` output: %q", strings.TrimSpace(string(out)))
	}
	return v, nil
}

// parseCodexVersionOutput extracts the semver from `codex --version` output
// ("codex-cli 0.144.6" → "0.144.6"). Returns "" when the shape is unknown.
func parseCodexVersionOutput(out string) string {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 || fields[0] != "codex-cli" {
		return ""
	}
	return fields[1]
}
