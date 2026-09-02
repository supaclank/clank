package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// PinnedClaudeVersion is the standalone authentication CLI installed on sprites.
// Keep it aligned with the agent runtime bundled by acptools' locked Claude SDK.
const PinnedClaudeVersion = "2.1.257"

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

// ClaudeVersion returns the standalone authentication CLI version from PATH,
// not the ACP adapter's bundled agent runtime version.
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
