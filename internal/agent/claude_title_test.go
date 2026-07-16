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

// TestClaudeBackendEmitsAITitleOnResume verifies a resumed session surfaces an
// ai-title already on disk from Open alone — no completed turn required.
//
// Regression: the title was read only in handleResult, i.e. at turn completion.
// When the turn that would have surfaced it never completed (machine idle-exit
// mid-turn, daemon restart, CLI stall), the title sat in the transcript forever
// while clients kept showing the raw first prompt.
func TestClaudeBackendEmitsAITitleOnResume(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	workDir := t.TempDir()
	projDir := mkClaudeProjectDir(t, configDir, workDir)

	const sessionID = "sess-title-003"
	const wantTitle = "Fix login button on mobile"

	writeSessionJSONL(t, projDir, sessionID, []map[string]any{
		{
			"type":      "user",
			"uuid":      "u-1",
			"timestamp": "2026-07-15T10:00:00Z",
			"sessionId": sessionID,
			"cwd":       workDir,
			"message":   map[string]any{"role": "user", "content": "the login button is broken on mobile"},
		},
		{
			"type":      "ai-title",
			"aiTitle":   wantTitle,
			"sessionId": sessionID,
		},
	})

	// Resume variant: session ID pre-seeded, Open only — no prompt, no turn.
	b := agent.NewClaudeCodeBackendForSession(workDir, sessionID)
	b.ClientFactory = func(opts ...claudecode.Option) claudecode.Client {
		return claudecode.NewClientWithTransport(newMockTransport(nil), opts...)
	}
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
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

// TestClaudeBackendRechecksAITitleAfterFastTurn verifies the title still
// surfaces when it lands in the transcript only after the turn's result.
//
// Regression: the CLI generates the title concurrently with the first turn, so
// a trivial prompt's ~3s turn finishes before the title call does. handleResult
// read the transcript once, found nothing, and a single-turn session never
// surfaced the title even though the CLI wrote it seconds later.
func TestClaudeBackendRechecksAITitleAfterFastTurn(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	workDir := t.TempDir()
	projDir := mkClaudeProjectDir(t, configDir, workDir)

	const sessionID = "sess-title-004"
	const wantTitle = "Unclear session content"

	firstPrompt := map[string]any{
		"type":      "user",
		"uuid":      "u-1",
		"timestamp": "2026-07-15T10:00:00Z",
		"sessionId": sessionID,
		"cwd":       workDir,
		"message":   map[string]any{"role": "user", "content": "omg omg omg omg"},
	}
	// No ai-title yet: the turn's result beats the CLI's concurrent titling.
	writeSessionJSONL(t, projDir, sessionID, []map[string]any{firstPrompt})

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
	b.AITitleRecheckDelay = 100 * time.Millisecond
	b.ClientFactory = func(opts ...claudecode.Option) claudecode.Client {
		return claudecode.NewClientWithTransport(transport, opts...)
	}
	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{Text: "omg omg omg omg"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	defer b.Stop()

	// Idle means handleResult ran (and found no title — none is on disk).
	waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// The CLI's title call completes after the turn: append the ai-title line.
	writeSessionJSONL(t, projDir, sessionID, []map[string]any{
		firstPrompt,
		{
			"type":      "ai-title",
			"aiTitle":   wantTitle,
			"sessionId": sessionID,
		},
	})

	evt := waitForEventType(t, b.Events(), agent.EventTitleChange, 5*time.Second)
	data, ok := evt.Data.(agent.TitleChangeData)
	if !ok {
		t.Fatalf("EventTitleChange carried %T, want TitleChangeData", evt.Data)
	}
	if data.Title != wantTitle {
		t.Fatalf("title = %q, want %q", data.Title, wantTitle)
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
