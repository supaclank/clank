package acp

// Replays sanitized real-adapter frames (docs/chat-client-spec/fixtures/acp)
// through the production reducer. These pin the adapter behaviors the
// backend depends on — delta chunking, pre-merged replay, late/meta-only
// updates dropping harmlessly — against the exact pinned adapter
// versions the fixtures were captured from. Personal paths, identifiers,
// command inventories, timestamps, and usage values are normalized.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/supaclank/clank/internal/agent"
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
