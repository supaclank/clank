package agent

import (
	"encoding/json"
	"testing"

	"github.com/pmenglund/codex-sdk-go/rpc"
)

// Notification payloads below are captured verbatim from a live codex
// app-server 0.144.6 session (trimmed to mapped fields), so the mapping is
// tested against the real wire shapes.

const codexTestThreadID = "019f84de-fbda-72f0-bc01-d9be4dfdb235"

func newTestCodexBackend() *CodexBackend {
	b := NewCodexBackendForSession("/tmp/work", codexTestThreadID)
	return b
}

// drainEvents collects everything currently buffered on the events channel.
func drainEvents(b *CodexBackend) []Event {
	var out []Event
	for {
		select {
		case ev := <-b.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func note(method, raw string) rpc.Notification {
	return rpc.Notification{Method: method, Raw: json.RawMessage(raw)}
}

func TestCodexTurnLifecycleMapping(t *testing.T) {
	t.Parallel()
	b := newTestCodexBackend()

	b.handleNotification(note("turn/started",
		`{"threadId":"`+codexTestThreadID+`","turn":{"id":"turn-1","status":"inProgress"}}`))

	evs := drainEvents(b)
	if len(evs) != 2 {
		t.Fatalf("turn/started: got %d events, want 2 (status + message shell): %+v", len(evs), evs)
	}
	if sc := evs[0].Data.(StatusChangeData); sc.NewStatus != StatusBusy {
		t.Errorf("status after turn/started = %s, want busy", sc.NewStatus)
	}
	msg := evs[1].Data.(MessageData)
	if msg.ID != "turn-1" || msg.Role != "assistant" || msg.ProviderID != codexProviderOpenAI {
		t.Errorf("assistant shell = %+v, want id=turn-1 role=assistant provider=openai", msg)
	}
	if evs[1].ExternalID != codexTestThreadID {
		t.Errorf("event ExternalID = %q, want thread id", evs[1].ExternalID)
	}

	b.handleNotification(note("item/agentMessage/delta",
		`{"threadId":"`+codexTestThreadID+`","itemId":"msg-1","delta":"BAN"}`))
	evs = drainEvents(b)
	if len(evs) != 1 {
		t.Fatalf("delta: got %d events, want 1", len(evs))
	}
	pu := evs[0].Data.(PartUpdateData)
	if !pu.IsDelta || pu.Part.Text != "BAN" || pu.Part.Type != PartText || pu.MessageID != "turn-1" {
		t.Errorf("delta part = %+v, want IsDelta text=BAN on message turn-1", pu)
	}

	// Completed command execution → tool part with output, completed status.
	b.handleNotification(note("item/completed",
		`{"threadId":"`+codexTestThreadID+`","item":{"type":"commandExecution","id":"exec-1","command":"/bin/zsh -lc './spike-echo.sh hello'","cwd":"/tmp/work","status":"completed","aggregatedOutput":"spike-approved: hello\n","exitCode":0}}`))
	evs = drainEvents(b)
	if len(evs) != 1 {
		t.Fatalf("commandExecution: got %d events, want 1", len(evs))
	}
	pu = evs[0].Data.(PartUpdateData)
	if pu.Part.Tool != codexToolShell || pu.Part.Status != PartCompleted {
		t.Errorf("tool part = %+v, want shell completed", pu.Part)
	}
	if pu.Part.Output != "spike-approved: hello\n" {
		t.Errorf("tool output = %q", pu.Part.Output)
	}
	if pu.Part.Input["command"] != "/bin/zsh -lc './spike-echo.sh hello'" {
		t.Errorf("tool input = %+v", pu.Part.Input)
	}

	b.handleNotification(note("turn/completed",
		`{"threadId":"`+codexTestThreadID+`","turn":{"id":"turn-1","status":"completed"}}`))
	evs = drainEvents(b)
	if len(evs) != 1 {
		t.Fatalf("turn/completed: got %d events, want 1", len(evs))
	}
	if sc := evs[0].Data.(StatusChangeData); sc.NewStatus != StatusIdle {
		t.Errorf("status after turn/completed = %s, want idle", sc.NewStatus)
	}
}

func TestCodexTurnFailureMapping(t *testing.T) {
	t.Parallel()
	b := newTestCodexBackend()

	b.handleNotification(note("turn/started",
		`{"threadId":"`+codexTestThreadID+`","turn":{"id":"turn-9","status":"inProgress"}}`))
	drainEvents(b)

	b.handleNotification(note("turn/completed",
		`{"threadId":"`+codexTestThreadID+`","turn":{"id":"turn-9","status":"failed","error":{"message":"model exploded"}}}`))
	evs := drainEvents(b)
	if len(evs) != 2 {
		t.Fatalf("failed turn: got %d events, want error+status: %+v", len(evs), evs)
	}
	if ed := evs[0].Data.(ErrorData); ed.Message != "model exploded" {
		t.Errorf("error message = %q", ed.Message)
	}
	if sc := evs[1].Data.(StatusChangeData); sc.NewStatus != StatusError {
		t.Errorf("status = %s, want error", sc.NewStatus)
	}
}

func TestCodexForeignThreadNotificationsDropped(t *testing.T) {
	t.Parallel()
	b := newTestCodexBackend()

	b.handleNotification(note("turn/started",
		`{"threadId":"other-thread","turn":{"id":"turn-x","status":"inProgress"}}`))
	if evs := drainEvents(b); len(evs) != 0 {
		t.Fatalf("foreign-thread notification produced events: %+v", evs)
	}
	if b.Status() != StatusStarting {
		t.Errorf("status = %s, want starting (untouched)", b.Status())
	}
}

func TestCodexUnknownMethodIgnored(t *testing.T) {
	t.Parallel()
	b := newTestCodexBackend()
	// Forward-compat: codex ships new notification methods every few days.
	b.handleNotification(note("thread/newFancyThing/updated",
		`{"threadId":"`+codexTestThreadID+`","stuff":true}`))
	if evs := drainEvents(b); len(evs) != 0 {
		t.Fatalf("unknown method produced events: %+v", evs)
	}
}

func TestCodexReasoningDeltaMapsToThinking(t *testing.T) {
	t.Parallel()
	b := newTestCodexBackend()
	b.handleNotification(note("turn/started",
		`{"threadId":"`+codexTestThreadID+`","turn":{"id":"turn-2","status":"inProgress"}}`))
	drainEvents(b)

	b.handleNotification(note("item/reasoning/summaryTextDelta",
		`{"threadId":"`+codexTestThreadID+`","itemId":"rs-1","delta":"pondering"}`))
	evs := drainEvents(b)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	pu := evs[0].Data.(PartUpdateData)
	if pu.Part.Type != PartThinking || pu.Part.Text != "pondering" || !pu.IsDelta {
		t.Errorf("thinking delta = %+v", pu)
	}
}
