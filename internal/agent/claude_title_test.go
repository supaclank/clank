package agent_test

import (
	"context"
	"testing"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"

	"github.com/acksell/clank/internal/agent"
)

// TestClaudeBackendEmitsAITitleFromTranscript verifies the backend surfaces the
// CLI-generated ai-title as an EventTitleChange once a turn completes.
//
// Regression: natively-created Claude sessions displayed the first prompt as
// their title because the backend never emitted a title event — the ai-title is
// written only to the on-disk transcript, so nothing in the live stdout stream
// the SDK parses ever carried it.
func TestClaudeBackendEmitsAITitleFromTranscript(t *testing.T) {
	// Cannot use t.Parallel: t.Setenv mutates process env.
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	workDir := t.TempDir()
	projDir := mkClaudeProjectDir(t, configDir, workDir)

	const sessionID = "sess-title-001"
	const wantTitle = "Check CLI version and model information"

	writeSessionJSONL(t, projDir, sessionID, []map[string]any{
		{
			"type":      "user",
			"uuid":      "u-1",
			"timestamp": "2026-04-25T10:00:00Z",
			"sessionId": sessionID,
			"cwd":       workDir,
			"message":   map[string]any{"role": "user", "content": "what version is this cli?"},
		},
		// The CLI appends the generated title as a bare metadata record with no
		// timestamp, right after the first prompt.
		{
			"type":      "ai-title",
			"aiTitle":   wantTitle,
			"sessionId": sessionID,
		},
	})

	b := newTitleTestBackend(t, workDir, sessionID)
	defer b.Stop()

	evt := waitForEventType(t, b.Events(), agent.EventTitleChange, 5*time.Second)
	data, ok := evt.Data.(agent.TitleChangeData)
	if !ok {
		t.Fatalf("EventTitleChange carried %T, want TitleChangeData", evt.Data)
	}
	if data.Title != wantTitle {
		t.Fatalf("title = %q, want %q", data.Title, wantTitle)
	}
}

// TestClaudeBackendNoAITitleNoEvent verifies a session whose transcript carries
// no ai-title yet emits no EventTitleChange — publishing an empty title would
// blank the first-prompt fallback clients render.
func TestClaudeBackendNoAITitleNoEvent(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	workDir := t.TempDir()
	projDir := mkClaudeProjectDir(t, configDir, workDir)

	const sessionID = "sess-title-002"
	writeSessionJSONL(t, projDir, sessionID, []map[string]any{
		{
			"type":      "user",
			"uuid":      "u-1",
			"timestamp": "2026-04-25T10:00:00Z",
			"sessionId": sessionID,
			"cwd":       workDir,
			"message":   map[string]any{"role": "user", "content": "hi"},
		},
	})

	b := newTitleTestBackend(t, workDir, sessionID)
	defer b.Stop()

	// Drive to idle collecting every event; none may be a title change.
	for _, evt := range waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second) {
		if evt.Type == agent.EventTitleChange {
			t.Fatalf("unexpected EventTitleChange with no ai-title on disk: %+v", evt.Data)
		}
	}
}

// newTitleTestBackend wires a backend to a replay transport that supplies the
// session ID (init) and completes one successful turn (result), so handleResult
// runs and reads the on-disk transcript for the ai-title.
func newTitleTestBackend(t *testing.T, workDir, sessionID string) *agent.ClaudeCodeBackend {
	t.Helper()
	result := "ok"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   sessionID,
			Result:      &result,
		},
	})

	b := agent.NewClaudeCodeBackend(workDir)
	b.ClientFactory = func(opts ...claudecode.Option) claudecode.Client {
		return claudecode.NewClientWithTransport(transport, opts...)
	}
	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{Text: "hi"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	return b
}
