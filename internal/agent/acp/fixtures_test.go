package acp

// Replays captured real-adapter frames (docs/chat-client-spec/fixtures/acp)
// through the production reducer. These pin the adapter behaviors the
// backend depends on — delta chunking, pre-merged replay, late/meta-only
// updates dropping harmlessly — against the exact pinned adapter
// versions the fixtures were captured from.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	sdk "github.com/coder/acp-go-sdk"
)

const fixtureDir = "../../../docs/chat-client-spec/fixtures/acp"

// loadFixtureUpdates parses every session/update line of a fixture.
func loadFixtureUpdates(t *testing.T, name string) []sdk.SessionNotification {
	t.Helper()
	f, err := os.Open(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	var out []sdk.SessionNotification
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var line struct {
			Kind    string          `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("parse fixture line: %v", err)
		}
		if line.Kind != "session/update" {
			continue
		}
		var n sdk.SessionNotification
		if err := json.Unmarshal(line.Payload, &n); err != nil {
			t.Fatalf("parse session/update payload: %v", err)
		}
		out = append(out, n)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("fixture contains no session/update frames")
	}
	return out
}

// reduceLive runs updates through a live-mode reducer with an open turn,
// then commits, returning (events, transcript).
func reduceLive(t *testing.T, updates []sdk.SessionNotification) ([]agent.Event, []agent.MessageData) {
	t.Helper()
	r := newReducer(t.Logf)
	r.setSessionID("fixture")
	r.beginTurn()
	var events []agent.Event
	for _, n := range updates {
		events = append(events, r.reduce(n)...)
	}
	events = append(events, r.finishTurn()...)
	return events, r.snapshot()
}

func assistantText(msgs []agent.MessageData) string {
	for _, m := range msgs {
		if m.Role == "assistant" {
			return m.Content
		}
	}
	return ""
}

func TestFixture_OpenCodeTurn(t *testing.T) {
	t.Parallel()
	events, msgs := reduceLive(t, loadFixtureUpdates(t, "opencode-1.17.18-turn.jsonl"))

	if got := assistantText(msgs); got != "SPIKE_OK" {
		t.Errorf("assistant content = %q, want SPIKE_OK", got)
	}
	// The turn streamed thinking then text — both as delta part updates.
	var sawThinking, sawText bool
	for _, e := range events {
		if e.Type != agent.EventPartUpdate {
			continue
		}
		p := e.Data.(agent.PartUpdateData)
		if !p.IsDelta {
			t.Errorf("opencode live text arrived as snapshot, want deltas: %+v", p.Part)
		}
		switch p.Part.Type {
		case agent.PartThinking:
			sawThinking = true
		case agent.PartText:
			sawText = true
		}
	}
	if !sawThinking || !sawText {
		t.Errorf("expected thinking+text deltas, got thinking=%v text=%v", sawThinking, sawText)
	}
	// usage_update and available_commands_update reduce to nothing.
	for _, e := range events {
		if e.Type == agent.EventMessage {
			md := e.Data.(agent.MessageData)
			if md.Role == "assistant" && (md.Content != "" || len(md.Parts) > 0) {
				t.Errorf("assistant message event must be a shell, got %+v", md)
			}
		}
	}
}

func TestFixture_OpenCodeLoadReplay(t *testing.T) {
	t.Parallel()
	updates := loadFixtureUpdates(t, "opencode-1.17.18-load-replay.jsonl")
	r := newReducer(t.Logf)
	r.setSessionID("fixture")
	r.replaying = true
	for _, n := range updates {
		if evts := r.reduce(n); len(evts) != 0 {
			t.Errorf("replay emitted %d live events, want none", len(evts))
		}
	}
	r.finishReplay()
	msgs := r.snapshot()
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("replayed transcript shape = %+v", msgs)
	}
	if msgs[0].Content != "Reply with exactly: SPIKE_OK" || msgs[1].Content != "SPIKE_OK" {
		t.Errorf("replayed contents = %q / %q", msgs[0].Content, msgs[1].Content)
	}
}

// The claude turn fixture carries the late-update class (title after the
// prompt resolved) and _meta-only update variants — both must pass or
// drop harmlessly, never wedge the stream.
func TestFixture_ClaudeTurnWithMetaAndLateTitle(t *testing.T) {
	t.Parallel()
	events, msgs := reduceLive(t, loadFixtureUpdates(t, "claude-agent-acp-0.61.0-turn.jsonl"))

	if got := assistantText(msgs); got != "SPIKE_OK" {
		t.Errorf("assistant content = %q, want SPIKE_OK", got)
	}
	titles := 0
	for _, e := range events {
		if e.Type == agent.EventTitleChange {
			titles++
			if e.Data.(agent.TitleChangeData).Title == "" {
				t.Error("empty title event")
			}
		}
	}
	if titles != 1 {
		t.Errorf("title events = %d, want 1 (session_info_update passes through)", titles)
	}
}

func TestFixture_ClaudeToolTurn(t *testing.T) {
	t.Parallel()
	_, msgs := reduceLive(t, loadFixtureUpdates(t, "claude-agent-acp-0.61.0-tool-turn.jsonl"))

	var call, result *agent.Part
	for i := range msgs {
		for j := range msgs[i].Parts {
			p := &msgs[i].Parts[j]
			switch p.Type {
			case agent.PartToolCall:
				call = p
			case agent.PartToolResult:
				result = p
			}
		}
	}
	if call == nil || result == nil {
		t.Fatalf("expected tool call + result in transcript, got call=%v result=%v", call, result)
	}
	if call.ID != result.ID {
		t.Errorf("tool parts must share one id: call=%q result=%q", call.ID, result.ID)
	}
	// The raw tool name rides _meta.claudeCode.toolName, not the title.
	if call.Tool != "Bash" {
		t.Errorf("tool name = %q, want Bash (from _meta.claudeCode.toolName)", call.Tool)
	}
	if !strings.Contains(result.Output, "SPIKE_PERM_OK") {
		t.Errorf("tool result output = %q, want the command output", result.Output)
	}
}

// The hermes turn fixture (hermes-agent 0.19.0 over a local
// OpenAI-compatible model) carries text deltas plus the late-title
// class: a session_info_update arriving after the prompt resolved.
func TestFixture_HermesTurn(t *testing.T) {
	t.Parallel()
	events, msgs := reduceLive(t, loadFixtureUpdates(t, "hermes-agent-0.19.0-turn.jsonl"))

	if got := assistantText(msgs); got != "SPIKE_OK" {
		t.Errorf("assistant content = %q, want SPIKE_OK", got)
	}
	for _, e := range events {
		if e.Type != agent.EventPartUpdate {
			continue
		}
		if p := e.Data.(agent.PartUpdateData); p.Part.Type == agent.PartText && !p.IsDelta {
			t.Errorf("hermes live text arrived as snapshot, want deltas: %+v", p.Part)
		}
	}
	titles := 0
	for _, e := range events {
		if e.Type == agent.EventTitleChange {
			titles++
			if e.Data.(agent.TitleChangeData).Title == "" {
				t.Error("empty title event")
			}
		}
	}
	if titles != 1 {
		t.Errorf("title events = %d, want 1 (late session_info_update passes through)", titles)
	}
}

func TestFixture_HermesLoadReplay(t *testing.T) {
	t.Parallel()
	updates := loadFixtureUpdates(t, "hermes-agent-0.19.0-load-replay.jsonl")
	r := newReducer(t.Logf)
	r.setSessionID("fixture")
	r.replaying = true
	for _, n := range updates {
		if evts := r.reduce(n); len(evts) != 0 {
			t.Errorf("replay emitted %d live events, want none", len(evts))
		}
	}
	r.finishReplay()
	msgs := r.snapshot()
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("replayed transcript shape = %+v", msgs)
	}
	if msgs[0].Content != "Reply with exactly: SPIKE_OK" || msgs[1].Content != "SPIKE_OK" {
		t.Errorf("replayed contents = %q / %q", msgs[0].Content, msgs[1].Content)
	}
}

// Hermes emits a tool_call for its edit tool (the permission prompt rides
// the conn layer, not session/update) and then NO tool_call_update — the
// call must land in the transcript and stay result-less without wedging
// the turn.
func TestFixture_HermesToolTurn(t *testing.T) {
	t.Parallel()
	_, msgs := reduceLive(t, loadFixtureUpdates(t, "hermes-agent-0.19.0-tool-turn.jsonl"))

	var call, result *agent.Part
	for i := range msgs {
		for j := range msgs[i].Parts {
			p := &msgs[i].Parts[j]
			switch p.Type {
			case agent.PartToolCall:
				call = p
			case agent.PartToolResult:
				result = p
			}
		}
	}
	if call == nil {
		t.Fatal("expected a tool call part in the transcript")
	}
	if result != nil {
		t.Errorf("hermes sends no tool_call_update; expected no result part, got %+v", result)
	}
	// No _meta tool name channel: the display title is the tool name.
	if call.Tool != "write: perm_test.txt" {
		t.Errorf("tool name = %q, want the title fallback", call.Tool)
	}
	if got := assistantText(msgs); !strings.Contains(got, "SPIKE_PERM_OK") {
		t.Errorf("assistant text = %q, want it to contain SPIKE_PERM_OK", got)
	}
}

// The pi turn fixture (pi-acp 0.0.32 driving pi 0.82.1 over a local
// OpenAI-compatible model) streams text deltas and session_info_updates
// both before and after the reply.
func TestFixture_PiTurn(t *testing.T) {
	t.Parallel()
	events, msgs := reduceLive(t, loadFixtureUpdates(t, "pi-acp-0.0.32-turn.jsonl"))

	if got := strings.TrimSpace(assistantText(msgs)); got != "SPIKE_OK" {
		t.Errorf("assistant content = %q, want SPIKE_OK", got)
	}
	for _, e := range events {
		if e.Type != agent.EventPartUpdate {
			continue
		}
		if p := e.Data.(agent.PartUpdateData); p.Part.Type == agent.PartText && !p.IsDelta {
			t.Errorf("pi live text arrived as snapshot, want deltas: %+v", p.Part)
		}
	}
}

func TestFixture_PiLoadReplay(t *testing.T) {
	t.Parallel()
	updates := loadFixtureUpdates(t, "pi-acp-0.0.32-load-replay.jsonl")
	r := newReducer(t.Logf)
	r.setSessionID("fixture")
	r.replaying = true
	for _, n := range updates {
		if evts := r.reduce(n); len(evts) != 0 {
			t.Errorf("replay emitted %d live events, want none", len(evts))
		}
	}
	r.finishReplay()
	msgs := r.snapshot()
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("replayed transcript shape = %+v", msgs)
	}
	if msgs[0].Content != "Reply with exactly: SPIKE_OK" || strings.TrimSpace(msgs[1].Content) != "SPIKE_OK" {
		t.Errorf("replayed contents = %q / %q", msgs[0].Content, msgs[1].Content)
	}
}

// pi runs its core tools with NO permission prompt (it has no permission
// system) — the write tool goes straight to tool_call + tool_call_update
// lifecycle frames, ending completed with diff content.
func TestFixture_PiToolTurn(t *testing.T) {
	t.Parallel()
	_, msgs := reduceLive(t, loadFixtureUpdates(t, "pi-acp-0.0.32-tool-turn.jsonl"))

	var call, result *agent.Part
	for i := range msgs {
		for j := range msgs[i].Parts {
			p := &msgs[i].Parts[j]
			switch p.Type {
			case agent.PartToolCall:
				call = p
			case agent.PartToolResult:
				result = p
			}
		}
	}
	if call == nil || result == nil {
		t.Fatalf("expected tool call + result, got call=%v result=%v", call, result)
	}
	if call.ID != result.ID {
		t.Errorf("tool parts must share one id: call=%q result=%q", call.ID, result.ID)
	}
	if call.Tool != "write" {
		t.Errorf("tool name = %q, want write (pi title)", call.Tool)
	}
	if call.Status != agent.PartCompleted {
		t.Errorf("tool status = %q, want completed", call.Status)
	}
	if !strings.Contains(result.Output, "SPIKE_PERM_OK") {
		t.Errorf("tool result output = %q, want the diff content", result.Output)
	}
}

func TestFixture_ClaudeLoadReplay(t *testing.T) {
	t.Parallel()
	updates := loadFixtureUpdates(t, "claude-agent-acp-0.61.0-load-replay.jsonl")
	r := newReducer(t.Logf)
	r.setSessionID("fixture")
	r.replaying = true
	for _, n := range updates {
		r.reduce(n)
	}
	r.finishReplay()
	msgs := r.snapshot()
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("replayed transcript shape = %+v", msgs)
	}
	if msgs[1].Content != "SPIKE_OK" {
		t.Errorf("replayed assistant = %q, want SPIKE_OK", msgs[1].Content)
	}
}
