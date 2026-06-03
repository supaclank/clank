package sessionsync

import (
	"context"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// TestPreparePush_DeltaOnly is the regression test for the "push re-exports
// every session even when nothing changed" bug: against a record that
// already reflects the worktree, PreparePush exports NOTHING, yet the
// manifest stays COMPLETE (so every checkpoint is a self-contained
// snapshot); changing one session re-exports exactly that one.
//
// Not parallel: isolates CLAUDE_CONFIG_DIR + PATH via t.Setenv.
func TestPreparePush_DeltaOnly(t *testing.T) {
	t.Setenv(envClaudeConfigDir, t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no opencode/git on PATH
	ctx := context.Background()

	wtDir := t.TempDir()
	const sessA, sessB = "delta-sess-aaaa", "delta-sess-bbbb"
	writeClaudeTranscript(t, wtDir, sessA)
	pathB := writeClaudeTranscript(t, wtDir, sessB)

	empty := agent.SyncedSessionRecord{Sessions: map[string]agent.SyncedSession{}}

	// First push: empty record ⇒ both sessions are unsynced ⇒ both exported,
	// and the rebuilt record reflects them.
	first, err := PreparePush(ctx, wtDir, empty, nil)
	if err != nil {
		t.Fatalf("first PreparePush: %v", err)
	}
	first.Cleanup()
	if len(first.Upload) != 2 || len(first.Entries) != 2 || len(first.Record.Sessions) != 2 {
		t.Fatalf("first push: upload=%d entries=%d record=%d, want 2/2/2",
			len(first.Upload), len(first.Entries), len(first.Record.Sessions))
	}

	// Second push against that record, nothing changed: ZERO exported, but
	// the manifest stays COMPLETE (both sessions referenced by their stored
	// content hash) and the record keeps both entries.
	second, err := PreparePush(ctx, wtDir, first.Record, nil)
	if err != nil {
		t.Fatalf("second PreparePush: %v", err)
	}
	second.Cleanup()
	if len(second.Upload) != 0 {
		t.Errorf("second push exported %d sessions, want 0 (nothing changed)", len(second.Upload))
	}
	if len(second.Entries) != 2 {
		t.Errorf("second push manifest has %d entries, want 2 (must stay complete)", len(second.Entries))
	}
	if len(second.Record.Sessions) != 2 {
		t.Errorf("second push record has %d sessions, want 2 (unchanged entries must survive)", len(second.Record.Sessions))
	}

	// Change session B (append a turn ⇒ new last-message uuid ⇒ fingerprint
	// advances). Now exactly ONE session re-exports; the manifest is still
	// complete; B's content hash advances.
	appendLines(t, pathB, `{"type":"assistant","sessionId":"`+sessB+`","cwd":"`+wtDir+`","uuid":"a-2","timestamp":"2026-04-25T11:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"more"}]}}`)
	third, err := PreparePush(ctx, wtDir, second.Record, nil)
	if err != nil {
		t.Fatalf("third PreparePush: %v", err)
	}
	third.Cleanup()
	if len(third.Upload) != 1 {
		t.Fatalf("after changing one session, exported %d, want exactly 1", len(third.Upload))
	}
	if third.Upload[0].Entry.ExternalID != sessB {
		t.Errorf("exported the wrong session: %q, want %q", third.Upload[0].Entry.ExternalID, sessB)
	}
	if len(third.Entries) != 2 {
		t.Errorf("third push manifest has %d entries, want 2 (complete)", len(third.Entries))
	}
	if before, after := second.Record.Sessions[sessB].ContentHash, third.Record.Sessions[sessB].ContentHash; before == after {
		t.Errorf("changed session content hash did not advance: still %q", after)
	}
}
