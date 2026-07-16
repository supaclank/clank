package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// PinnedClaudeVersion is the Claude Code CLI version clank ships
// against. Bumping this constant is a deliberate, reviewable change —
// it determines what every fly.io provisioner installs onto a sprite.
//
// Why pin: the sprite base image bakes its own claude CLI with
// auto-updates disabled, frozen at whatever was current when the
// image was built. The CLI is not just a runtime: the family aliases
// clank passes as models (sonnet / opus / haiku / fable — see
// internal/host/backends.go) resolve to a CONCRETE model inside the
// CLI binary, so a stale claude silently downgrades every session's
// model. Seen 2026-07-05: an image-baked 2.1.168 resolved `sonnet`
// to Sonnet 4.6 well after newer families shipped, and that CLI
// vintage also retry-looped truncated thinking-only turns, burning
// 32k output tokens per attempt without ever calling a tool.
//
// The pinned value must be a published @anthropic-ai/claude-code npm
// version — the sprite-side installer feeds it to
// `bun install -g @anthropic-ai/claude-code@<pin>` and hard-fails on
// a version mismatch.
//
// Bumping this:
//  1. Update the constant.
//  2. `make install` — laptops get the new clank that knows the new pin.
//  3. Sprites probe-and-reinstall on next EnsureHost.
//
// Unlike PinnedOpencodeVersion there is no laptop-side compat gate:
// claude session blobs never round-trip through clank migrations, so
// drift is a quality problem (wrong model), not a corruption problem.
const PinnedClaudeVersion = "2.1.201"

// ParseClaudeVersionOutput extracts the bare version from `claude
// --version` output. The CLI prints a suffixed form ("2.1.201
// (Claude Code)"), so exact-matching raw output against
// PinnedClaudeVersion would never succeed; callers compare the first
// whitespace-delimited field instead. Returns "" for empty or
// all-whitespace input.
func ParseClaudeVersionOutput(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// ClaudeVersion runs `claude --version` and returns the parsed bare
// version (e.g. "2.1.201"). The binary is resolved via PATH — the
// same lookup the claude-agent-sdk uses to spawn session CLIs — so
// the reported version is the one sessions will actually run.
//
// The subprocess is called with no special env so it inherits
// clank-host's environment (HOME etc.). Reading the version doesn't
// touch session storage, so isolation isn't required.
func ClaudeVersion(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "claude", "--version")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		stderr := ""
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		return "", fmt.Errorf("claude --version: %w: %s", err, stderr)
	}
	v := ParseClaudeVersionOutput(string(out))
	if v == "" {
		return "", fmt.Errorf("claude --version: empty output")
	}
	return v, nil
}
