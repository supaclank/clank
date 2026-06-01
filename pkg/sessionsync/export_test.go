package sessionsync

import (
	"context"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// TestExportWorktreeSessions_DiscoversClaude proves Claude sessions are
// exported even when the opencode binary is absent (PATH stripped) — the
// missing-binary case no longer short-circuits the whole export, and the
// SessionEntry carries the Claude backend + the session id as both
// SessionID and ExternalID (daemon-free has no separate clank ULID).
//
// Not parallel: isolates CLAUDE_CONFIG_DIR and PATH via t.Setenv.
func TestExportWorktreeSessions_DiscoversClaude(t *testing.T) {
	t.Setenv(envClaudeConfigDir, t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no opencode (and no git) on PATH
	ctx := context.Background()

	wtDir := t.TempDir()
	const sessionID = "export-wt-claude-7777"
	writeClaudeTranscript(t, wtDir, sessionID)

	res, err := ExportWorktreeSessions(ctx, wtDir)
	if err != nil {
		t.Fatalf("ExportWorktreeSessions: %v", err)
	}
	t.Cleanup(res.Cleanup)

	if len(res.Exported) != 1 {
		t.Fatalf("want 1 exported claude session, got %d (opencode-absent must not hide Claude)", len(res.Exported))
	}
	e := res.Exported[0].Entry
	if e.Backend != agent.BackendClaudeCode {
		t.Errorf("Backend = %q, want claude-code", e.Backend)
	}
	if e.ExternalID != sessionID {
		t.Errorf("ExternalID = %q, want %q", e.ExternalID, sessionID)
	}
	if e.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q (= ExternalID in daemon-free mode)", e.SessionID, sessionID)
	}
	if e.Bytes <= 0 {
		t.Errorf("Bytes = %d, want positive", e.Bytes)
	}
	// The content fingerprint (last-message uuid) is carried for the
	// last-pushed record so later drift detection ignores mtime-only bumps.
	if got := res.Exported[0].Fingerprint; got != "a-1" {
		t.Errorf("Fingerprint = %q, want a-1 (last uuid in the fixture)", got)
	}
}
