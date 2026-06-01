package sessionsync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Claude Code stores each session as a JSONL transcript at
// <configDir>/projects/<encodeClaudeCwd(abs(cwd))>/<sessionID>.jsonl.
// These helpers are the single source of truth for that layout. They
// mirror the Agent SDK's internal (unexported) path logic exactly; the
// SDK-anchored test in claude_paths_test.go writes a transcript via
// claudeTranscriptPath and asserts claudecode.ListSessions discovers it,
// so any drift from the SDK's encoding fails loudly.
const (
	envClaudeConfigDir   = "CLAUDE_CONFIG_DIR"
	claudeConfigDirName  = ".claude"
	claudeProjectsSubdir = "projects"
	claudeTranscriptExt  = ".jsonl"
)

// claudeConfigDir resolves Claude Code's config dir, honoring
// CLAUDE_CONFIG_DIR and falling back to ~/.claude. Mirrors the SDK's
// internal configDir().
func claudeConfigDir() (string, error) {
	if d := os.Getenv(envClaudeConfigDir); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, claudeConfigDirName), nil
}

// encodeClaudeCwd replaces every non-alphanumeric rune with "-",
// matching how Claude Code names the per-project transcript directory.
// Pure rune replacement (no Abs) to mirror the SDK's encodeCwd; callers
// pass an already-absolute path.
func encodeClaudeCwd(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// claudeTranscriptPath returns the on-disk JSONL path for a session whose
// working directory is cwd. cwd is resolved to an absolute path first, the
// same as the SDK does before encoding.
func claudeTranscriptPath(cwd, sessionID string) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("claude transcript path: cwd is required")
	}
	if sessionID == "" {
		return "", fmt.Errorf("claude transcript path: sessionID is required")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("claude transcript path: resolve cwd %q: %w", cwd, err)
	}
	cfg, err := claudeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, claudeProjectsSubdir, encodeClaudeCwd(abs), sessionID+claudeTranscriptExt), nil
}
