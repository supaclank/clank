package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"

	"github.com/acksell/clank/internal/agent"
)

// TestClaudeBackendMessagesFromDisk verifies that ClaudeCodeBackend.Messages
// returns conversation history reconstructed from Claude Code's on-disk JSONL
// transcript via the SDK's GetSessionMessages. This is the source of truth
// for history reload (TUI reopen, daemon restart) — the streaming path no
// longer accumulates into an in-memory buffer.
//
// The test writes a JSONL fixture under a CLAUDE_CONFIG_DIR-pointed temp dir
// at the path the SDK expects (~/.claude/projects/<encoded-cwd>/<id>.jsonl)
// and then exercises Messages() through a real backend wired to a mock
// transport that supplies the session ID via a SystemMessage init.
func TestClaudeBackendMessagesFromDisk(t *testing.T) {
	// Cannot use t.Parallel because t.Setenv mutates process env.
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	workDir := t.TempDir()
	projDir := mkClaudeProjectDir(t, configDir, workDir)

	const sessionID = "sess-disk-001"
	const apiMsgID = "msg_api_001"
	const apiMsgID2 = "msg_api_002"
	const toolUseID = "toolu_disk_001"

	// JSONL fixture mimicking the on-disk transcript Claude Code writes.
	// Includes: string-content user msg, assistant text+thinking, tool_use,
	// tool_result, and a follow-up assistant text in a second API message.
	writeSessionJSONL(t, projDir, sessionID, []map[string]any{
		// Filtered (meta) entry — must be skipped by Messages().
		{
			"type":      "queue-operation",
			"timestamp": "2026-04-25T10:00:00Z",
			"sessionId": sessionID,
		},
		// User: string content.
		{
			"type":      "user",
			"uuid":      "u-1",
			"timestamp": "2026-04-25T10:00:01Z",
			"sessionId": sessionID,
			"cwd":       workDir,
			"message": map[string]any{
				"role":    "user",
				"content": "Run pwd and tell me where we are.",
			},
		},
		// Assistant: thinking + text + tool_use blocks under a single API msg id.
		{
			"type":      "assistant",
			"uuid":      "a-1",
			"timestamp": "2026-04-25T10:00:02Z",
			"sessionId": sessionID,
			"message": map[string]any{
				"id":    apiMsgID,
				"model": "claude-sonnet-4",
				"role":  "assistant",
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "I should run pwd."},
					map[string]any{"type": "text", "text": "Let me check."},
					map[string]any{
						"type":  "tool_use",
						"id":    toolUseID,
						"name":  "Bash",
						"input": map[string]any{"command": "pwd"},
					},
				},
			},
		},
		// User: tool_result block (Claude Code records tool results as user msgs).
		{
			"type":      "user",
			"uuid":      "u-2",
			"timestamp": "2026-04-25T10:00:03Z",
			"sessionId": sessionID,
			"message": map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": toolUseID,
						"content":     "/home/user/proj",
					},
				},
			},
		},
		// Assistant: follow-up text in a *different* API message (id changes).
		{
			"type":      "assistant",
			"uuid":      "a-2",
			"timestamp": "2026-04-25T10:00:04Z",
			"sessionId": sessionID,
			"message": map[string]any{
				"id":    apiMsgID2,
				"model": "claude-sonnet-4",
				"role":  "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "You are at /home/user/proj."},
				},
			},
		},
	})

	b := newBackendForDir(t, workDir, sessionID)
	defer b.Stop()

	msgs, err := b.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}

	if got, want := len(msgs), 4; got != want {
		for i, m := range msgs {
			t.Logf("msg %d: role=%s content=%q parts=%d", i, m.Role, m.Content, len(m.Parts))
		}
		t.Fatalf("got %d messages, want %d", got, want)
	}

	// 0: user, string content.
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want user", msgs[0].Role)
	}
	if msgs[0].Content != "Run pwd and tell me where we are." {
		t.Errorf("msgs[0].Content = %q", msgs[0].Content)
	}
	if len(msgs[0].Parts) != 0 {
		t.Errorf("msgs[0].Parts = %d, want 0 (string content)", len(msgs[0].Parts))
	}

	// 1: assistant, thinking + text + tool_use.
	a1 := msgs[1]
	if a1.Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want assistant", a1.Role)
	}
	if a1.ID != apiMsgID {
		t.Errorf("msgs[1].ID = %q, want %q (Anthropic API msg id)", a1.ID, apiMsgID)
	}
	if a1.ModelID != "claude-sonnet-4" {
		t.Errorf("msgs[1].ModelID = %q", a1.ModelID)
	}
	if len(a1.Parts) != 3 {
		t.Fatalf("msgs[1].Parts = %d, want 3", len(a1.Parts))
	}
	// thinking at index 0 → ID "{apiMsgID}-0" (matches blockID()).
	if a1.Parts[0].Type != agent.PartThinking {
		t.Errorf("Parts[0].Type = %q, want thinking", a1.Parts[0].Type)
	}
	if a1.Parts[0].ID != apiMsgID+"-0" {
		t.Errorf("Parts[0].ID = %q, want %q-0", a1.Parts[0].ID, apiMsgID)
	}
	if a1.Parts[0].Text != "I should run pwd." {
		t.Errorf("Parts[0].Text = %q", a1.Parts[0].Text)
	}
	// text at index 1 → ID "{apiMsgID}-1".
	if a1.Parts[1].Type != agent.PartText {
		t.Errorf("Parts[1].Type = %q, want text", a1.Parts[1].Type)
	}
	if a1.Parts[1].ID != apiMsgID+"-1" {
		t.Errorf("Parts[1].ID = %q, want %q-1", a1.Parts[1].ID, apiMsgID)
	}
	// tool_use at index 2 → ID is the tool_use_id, status completed.
	tu := a1.Parts[2]
	if tu.Type != agent.PartToolCall {
		t.Errorf("Parts[2].Type = %q, want tool_call", tu.Type)
	}
	if tu.ID != toolUseID {
		t.Errorf("Parts[2].ID = %q, want %q", tu.ID, toolUseID)
	}
	if tu.Tool != "Bash" {
		t.Errorf("Parts[2].Tool = %q, want Bash", tu.Tool)
	}
	if tu.Status != agent.PartCompleted {
		t.Errorf("Parts[2].Status = %q, want completed (no spinner on reload)", tu.Status)
	}
	if tu.Input["command"] != "pwd" {
		t.Errorf("Parts[2].Input[command] = %v", tu.Input["command"])
	}
	if tu.Text != "" {
		t.Errorf("Parts[2].Text should be empty on reload, got %q", tu.Text)
	}

	// 2: user, tool_result (no string content; one part).
	a2 := msgs[2]
	if a2.Role != "user" {
		t.Errorf("msgs[2].Role = %q, want user", a2.Role)
	}
	if len(a2.Parts) != 1 {
		t.Fatalf("msgs[2].Parts = %d, want 1", len(a2.Parts))
	}
	tr := a2.Parts[0]
	if tr.Type != agent.PartToolResult {
		t.Errorf("tool_result Type = %q", tr.Type)
	}
	if tr.ID != toolUseID {
		t.Errorf("tool_result ID = %q, want %q (paired with tool_use)", tr.ID, toolUseID)
	}
	if tr.Status != agent.PartCompleted {
		t.Errorf("tool_result Status = %q, want completed", tr.Status)
	}
	if tr.Output != "/home/user/proj" {
		t.Errorf("tool_result Output = %q", tr.Output)
	}

	// 3: assistant, text in a *new* API message — must get a different ID prefix
	// than msgs[1], proving message-scoped IDs are preserved even across cycles.
	a3 := msgs[3]
	if a3.ID != apiMsgID2 {
		t.Errorf("msgs[3].ID = %q, want %q", a3.ID, apiMsgID2)
	}
	if len(a3.Parts) != 1 {
		t.Fatalf("msgs[3].Parts = %d, want 1", len(a3.Parts))
	}
	if a3.Parts[0].ID != apiMsgID2+"-0" {
		t.Errorf("Parts[0].ID = %q, want %q-0 (new msg cycle)", a3.Parts[0].ID, apiMsgID2)
	}
	if strings.HasPrefix(a3.Parts[0].ID, apiMsgID+"-") {
		t.Errorf("Parts[0].ID %q collides with prior cycle id prefix %q", a3.Parts[0].ID, apiMsgID)
	}
}

// TestClaudeBackendMessagesCoalescesSplitRecords is the regression test for the
// "thinking bundled away from its tool calls / thinking dropped, tools only"
// bug. Claude Code does not write one JSONL record per message: it splits an
// assistant turn into several records that each hold a single content block but
// all share the turn's message.id (a thinking record, a text record, then one
// record per tool_use). Mapping each record 1:1 emitted several MessageData
// with the same id and colliding "{msgID}-0" part ids (block index restarts at
// 0 per record), which clients that group parts by message id render as
// segregated or dropped thinking. Messages() must coalesce the run into ONE
// assistant message with its blocks in order and a running block index.
func TestClaudeBackendMessagesCoalescesSplitRecords(t *testing.T) {
	// Cannot use t.Parallel because t.Setenv mutates process env.
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	workDir := t.TempDir()
	projDir := mkClaudeProjectDir(t, configDir, workDir)

	const sessionID = "sess-split-001"
	const apiMsgID = "msg_api_split"
	const toolUseID1 = "toolu_split_001"
	const toolUseID2 = "toolu_split_002"

	// One assistant turn, split across four records all sharing apiMsgID —
	// exactly how Claude Code writes a thinking + text + two-tool turn to disk.
	mkRecord := func(uuid string, block map[string]any) map[string]any {
		return map[string]any{
			"type":      "assistant",
			"uuid":      uuid,
			"timestamp": "2026-04-25T10:00:02Z",
			"sessionId": sessionID,
			"message": map[string]any{
				"id":      apiMsgID,
				"model":   "claude-sonnet-4",
				"role":    "assistant",
				"content": []any{block},
			},
		}
	}
	writeSessionJSONL(t, projDir, sessionID, []map[string]any{
		{
			"type":      "user",
			"uuid":      "u-1",
			"timestamp": "2026-04-25T10:00:01Z",
			"sessionId": sessionID,
			"cwd":       workDir,
			"message":   map[string]any{"role": "user", "content": "Build the thing."},
		},
		mkRecord("a-think", map[string]any{"type": "thinking", "thinking": "Planning the build."}),
		mkRecord("a-text", map[string]any{"type": "text", "text": "Working on it."}),
		mkRecord("a-tool1", map[string]any{
			"type": "tool_use", "id": toolUseID1, "name": "Write",
			"input": map[string]any{"file_path": "a.ts"},
		}),
		mkRecord("a-tool2", map[string]any{
			"type": "tool_use", "id": toolUseID2, "name": "Write",
			"input": map[string]any{"file_path": "b.ts"},
		}),
	})

	b := newBackendForDir(t, workDir, sessionID)
	defer b.Stop()

	msgs, err := b.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}

	// The four split records must coalesce into a single assistant message,
	// leaving exactly [user, assistant].
	if got, want := len(msgs), 2; got != want {
		for i, m := range msgs {
			t.Logf("msg %d: role=%s id=%s parts=%d", i, m.Role, m.ID, len(m.Parts))
		}
		t.Fatalf("got %d messages, want %d (split records must coalesce)", got, want)
	}

	a := msgs[1]
	if a.Role != "assistant" || a.ID != apiMsgID {
		t.Fatalf("msgs[1] role=%q id=%q, want assistant %q", a.Role, a.ID, apiMsgID)
	}
	if len(a.Parts) != 4 {
		t.Fatalf("msgs[1].Parts = %d, want 4 (thinking, text, 2 tools interleaved)", len(a.Parts))
	}

	// Blocks keep API order with a running index: thinking -0, text -1.
	if a.Parts[0].Type != agent.PartThinking || a.Parts[0].ID != apiMsgID+"-0" {
		t.Errorf("Parts[0] = {%q,%q}, want {thinking,%q-0}", a.Parts[0].Type, a.Parts[0].ID, apiMsgID)
	}
	if a.Parts[1].Type != agent.PartText || a.Parts[1].ID != apiMsgID+"-1" {
		t.Errorf("Parts[1] = {%q,%q}, want {text,%q-1}", a.Parts[1].Type, a.Parts[1].ID, apiMsgID)
	}
	// Thinking and text must NOT collide on "{msgID}-0".
	if a.Parts[0].ID == a.Parts[1].ID {
		t.Errorf("thinking and text share part id %q (block index did not advance across records)", a.Parts[0].ID)
	}
	if a.Parts[2].Type != agent.PartToolCall || a.Parts[2].ID != toolUseID1 {
		t.Errorf("Parts[2] = {%q,%q}, want {tool_call,%q}", a.Parts[2].Type, a.Parts[2].ID, toolUseID1)
	}
	if a.Parts[3].Type != agent.PartToolCall || a.Parts[3].ID != toolUseID2 {
		t.Errorf("Parts[3] = {%q,%q}, want {tool_call,%q}", a.Parts[3].Type, a.Parts[3].ID, toolUseID2)
	}
}

// TestClaudeBackendMessagesNoSessionID asserts that Messages() returns
// (nil, nil) before a session ID has been observed, instead of erroring or
// returning a stale buffer. This matches the contract documented on the
// Messages() method.
func TestClaudeBackendMessagesNoSessionID(t *testing.T) {
	t.Parallel()

	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()

	msgs, err := b.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if msgs != nil {
		t.Errorf("Messages() before session id = %v, want nil", msgs)
	}
}

// TestClaudeBackendMessagesResumeWithoutStart is the regression test for the
// "Waiting for agent output..." bug on reopening Claude sessions. The hub's
// activateBackend path constructs a backend via the manager but only calls
// Watch (a no-op for Claude); Start never runs. Before the fix, b.sessionID
// stayed empty and Messages() returned nil, leaving the TUI without history
// to render. The fix is NewClaudeCodeBackendForSession, which seeds sessionID
// at construction so Messages() can read the on-disk transcript without Start.
func TestClaudeBackendMessagesResumeWithoutStart(t *testing.T) {
	// Cannot use t.Parallel because t.Setenv mutates process env.
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	workDir := t.TempDir()
	projDir := mkClaudeProjectDir(t, configDir, workDir)

	const sessionID = "sess-resume-001"
	writeSessionJSONL(t, projDir, sessionID, []map[string]any{
		{"type": "queue-operation", "timestamp": "2026-04-25T10:00:00Z", "sessionId": sessionID},
		{
			"type":      "user",
			"uuid":      "u-1",
			"timestamp": "2026-04-25T10:00:01Z",
			"sessionId": sessionID,
			"cwd":       workDir,
			"message":   map[string]any{"role": "user", "content": "Old prompt"},
		},
		{
			"type":      "assistant",
			"uuid":      "a-1",
			"timestamp": "2026-04-25T10:00:02Z",
			"sessionId": sessionID,
			"message": map[string]any{
				"id":      "msg_old",
				"model":   "claude-sonnet-4",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": "Old reply"}},
			},
		},
	})

	// Construct exactly like ClaudeBackendManager.CreateBackend does on the
	// activateBackend path: pre-seeded session id, no Start, no Watch, no
	// transport, no client. Messages() must still return history.
	b := agent.NewClaudeCodeBackendForSession(workDir, sessionID)
	defer b.Stop()

	if got := b.SessionID(); got != sessionID {
		t.Fatalf("SessionID() = %q, want %q (must be set at construction)", got, sessionID)
	}

	msgs, err := b.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (history must be readable without Start)", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "Old prompt" {
		t.Errorf("msgs[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || len(msgs[1].Parts) != 1 || msgs[1].Parts[0].Text != "Old reply" {
		t.Errorf("msgs[1] = %+v", msgs[1])
	}
}

// TestClaudeBackendMessagesOversizedTranscriptLine is the regression test for
// sessions becoming permanently unreadable after the agent reads a fetched
// PDF: the CLI embeds the whole document as one base64 line in the transcript
// (a 1MB PDF ≈ a 1.37MB line), and the SDK's capped bufio.Scanner made every
// subsequent GetSessionMessages fail with "token too long" — surfacing as
// HTTP 500 on /sessions/{id}/messages and an empty conversation in clients.
// Messages() must return the full conversation, with the document block
// dropped (Part has no binary field) rather than the read failing.
func TestClaudeBackendMessagesOversizedTranscriptLine(t *testing.T) {
	// Cannot use t.Parallel because t.Setenv mutates process env.
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	workDir := t.TempDir()
	projDir := mkClaudeProjectDir(t, configDir, workDir)

	const sessionID = "sess-bigline-001"
	// ~2MB of base64-looking payload, generated at runtime — well past the
	// SDK's old 1MB scanner cap, no fixture file checked in.
	bigData := strings.Repeat("JVBERi0xLjYNJeLj", 128*1024)
	writeSessionJSONL(t, projDir, sessionID, []map[string]any{
		{
			"type":      "user",
			"uuid":      "u-1",
			"timestamp": "2026-07-10T16:04:00Z",
			"sessionId": sessionID,
			"cwd":       workDir,
			"message":   map[string]any{"role": "user", "content": "Read the rules PDF"},
		},
		// The oversized record: a user message carrying the PDF document block.
		{
			"type":      "user",
			"uuid":      "u-2",
			"timestamp": "2026-07-10T16:04:17Z",
			"sessionId": sessionID,
			"message": map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "document",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "application/pdf",
							"data":       bigData,
						},
					},
				},
			},
		},
		{
			"type":      "assistant",
			"uuid":      "a-1",
			"timestamp": "2026-07-10T16:04:30Z",
			"sessionId": sessionID,
			"message": map[string]any{
				"id":      "msg_after_pdf",
				"model":   "claude-sonnet-4",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": "Read it."}},
			},
		},
	})

	b := agent.NewClaudeCodeBackendForSession(workDir, sessionID)
	defer b.Stop()

	msgs, err := b.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages: %v (oversized transcript line must not fail the read)", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	// The document block is dropped, not forwarded: no binary over the wire.
	if msgs[1].Content != "" || len(msgs[1].Parts) != 0 {
		t.Errorf("msgs[1] (document carrier) = %+v, want empty content and no parts", msgs[1])
	}
	if msgs[2].Role != "assistant" || len(msgs[2].Parts) != 1 || msgs[2].Parts[0].Text != "Read it." {
		t.Errorf("msgs[2] = %+v (conversation after the oversized line must survive)", msgs[2])
	}
}

// --- Helpers ---

// mkClaudeProjectDir creates the per-cwd project directory inside a
// CLAUDE_CONFIG_DIR-pointed config dir, mirroring the SDK's encodeCwd
// (replace every non-alphanumeric rune with "-" after Abs).
func mkClaudeProjectDir(t *testing.T, configDir, cwd string) string {
	t.Helper()
	abs, err := filepath.Abs(cwd)
	if err != nil {
		t.Fatalf("Abs(%q): %v", cwd, err)
	}
	encoded := encodeCwdLikeSDK(abs)
	dir := filepath.Join(configDir, "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

// encodeCwdLikeSDK mirrors the SDK's unexported encodeCwd: every non
// alphanumeric rune becomes "-". Kept in sync with
// claude-agent-sdk-go/internal/session/session.go encodeCwd.
func encodeCwdLikeSDK(cwd string) string {
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

// writeSessionJSONL writes one JSON object per line to <dir>/<sessionID>.jsonl.
func writeSessionJSONL(t *testing.T, dir, sessionID string, entries []map[string]any) {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode entry: %v", err)
		}
	}
}

// newBackendForDir constructs a ClaudeCodeBackend pinned to workDir, drives
// it through Start with a mock transport that supplies sessionID via the
// init SystemMessage, and waits for the status to settle to idle.
//
// We need to go through Start so that handleSystemMessage populates
// b.sessionID — Messages() reads from disk only when a session ID is set.
func newBackendForDir(t *testing.T, workDir, sessionID string) *agent.ClaudeCodeBackend {
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

	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{
		Text: "hello",
	}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)
	return b
}

// While a permission is parked on handleCanUseTool, the gated tool_use block
// hasn't been flushed to the transcript — Messages must synthesize the
// in-flight tool part (with its question tag) so a client that (re)joins
// mid-park can render and answer the prompt. Regression: reopening a session
// blocked on AskUserQuestion showed no card and no permission prompt.
func TestClaudeBackendMessages_ParkedPermissionSynthesized(t *testing.T) {
	// Cannot use t.Parallel because t.Setenv mutates process env.
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	workDir := t.TempDir()
	projDir := mkClaudeProjectDir(t, configDir, workDir)

	const sessionID = "sess-parked-001"
	userEntry := map[string]any{
		"type":      "user",
		"uuid":      "u-1",
		"timestamp": "2026-07-15T10:00:00Z",
		"sessionId": sessionID,
		"cwd":       workDir,
		"message":   map[string]any{"role": "user", "content": "Set up auth."},
	}
	writeSessionJSONL(t, projDir, sessionID, []map[string]any{userEntry})

	transport := newMockTransport([]claudecode.Message{
		&claudecode.StreamEvent{Event: map[string]any{
			"type":  "content_block_start",
			"index": float64(0),
			"content_block": map[string]any{
				"type": "tool_use",
				"id":   "toolu_ask",
				"name": "AskUserQuestion",
			},
		}},
	})
	b := agent.NewClaudeCodeBackendForSession(workDir, sessionID)
	defer b.Stop()
	resolved := captureOpenOptions(t, b, transport)
	waitForToolPart(t, b.Events(), "AskUserQuestion", 2*time.Second)

	input := map[string]any{"questions": []any{map[string]any{
		"question": "Which auth?",
		"header":   "Auth",
		"options":  []any{map[string]any{"label": "JWT"}, map[string]any{"label": "Session"}},
	}}}
	done := make(chan struct{})
	go func() {
		_, _ = resolved.CanUseTool(context.Background(), "AskUserQuestion", input, nil)
		close(done)
	}()
	evt := waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second)
	perm := evt.Data.(agent.PermissionData)

	msgs, err := b.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	last := msgs[len(msgs)-1]
	if last.ID != "pending-"+perm.RequestID || last.Role != "assistant" {
		t.Fatalf("last message = {ID:%q Role:%q}, want the synthetic pending message", last.ID, last.Role)
	}
	if len(last.Parts) != 1 {
		t.Fatalf("synthetic message has %d parts, want 1", len(last.Parts))
	}
	part := last.Parts[0]
	if part.ID != "toolu_ask" || part.Tool != "AskUserQuestion" || part.Status != agent.PartRunning {
		t.Errorf("part = {ID:%q Tool:%q Status:%q}, want the parked tool_use, running", part.ID, part.Tool, part.Status)
	}
	if part.Question == nil {
		t.Fatal("part.Question is nil; the question card can't render from restored history")
	}
	// The tag must address the parked permission, not the "q-" bypass id —
	// a bypass-routed answer would Send into a session blocked on the park.
	if part.Question.RequestID != perm.RequestID {
		t.Errorf("Question.RequestID = %q, want %q", part.Question.RequestID, perm.RequestID)
	}
	if len(part.Question.Questions) != 1 || len(part.Question.Questions[0].Options) != 2 {
		t.Errorf("Question tag = %+v, want the parsed prompt", part.Question)
	}

	// Once the transcript contains the tool_use block (CLI flushed it), the
	// synthetic copy must not duplicate it.
	writeSessionJSONL(t, projDir, sessionID, []map[string]any{
		userEntry,
		{
			"type":      "assistant",
			"uuid":      "a-1",
			"timestamp": "2026-07-15T10:00:05Z",
			"sessionId": sessionID,
			"message": map[string]any{
				"id":    "msg_api_1",
				"model": "claude-sonnet-4",
				"role":  "assistant",
				"content": []any{map[string]any{
					"type":  "tool_use",
					"id":    "toolu_ask",
					"name":  "AskUserQuestion",
					"input": input,
				}},
			},
		},
	})
	msgs, err = b.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages (flushed): %v", err)
	}
	for _, m := range msgs {
		if strings.HasPrefix(m.ID, "pending-") {
			t.Errorf("synthetic message %q still present after the tool_use reached the transcript", m.ID)
		}
	}

	// Answering retires the synthetic message entirely.
	writeSessionJSONL(t, projDir, sessionID, []map[string]any{userEntry})
	if err := b.RespondPermission(context.Background(), perm.RequestID, false, "answered"); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not return after RespondPermission")
	}
	deadline := time.After(2 * time.Second)
	for {
		msgs, err = b.Messages(context.Background())
		if err != nil {
			t.Fatalf("Messages (after reply): %v", err)
		}
		if len(msgs) == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("Messages still returns %d messages after reply, want 1", len(msgs))
		case <-time.After(10 * time.Millisecond):
		}
	}
}
