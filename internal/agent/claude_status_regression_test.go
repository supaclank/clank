package agent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"

	"github.com/acksell/clank/internal/agent"
)

// TestOpenAndSend_BadAttachmentLeavesStatusIdle guards against a regression
// where a user-supplied bad attachment (wrong mime, oversized, bad URL) would
// flip a newly-opened session to StatusError, making it unrecoverable. The
// session must stay at StatusIdle so the user can retry.
func TestOpenAndSend_BadAttachmentLeavesStatusIdle(t *testing.T) {
	t.Parallel()
	tr := newMockTransport(nil)
	b := newTestBackend(t, tr)
	defer b.Stop()

	err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{
		Text: "hello",
		Attachments: []agent.Attachment{{
			Mime:   "image/svg+xml", // not in AllowedMimes → resolveImage returns error immediately
			Source: "data:image/svg+xml;base64,PHN2Zy8+",
		}},
	})
	if err == nil {
		t.Fatal("expected error from bad attachment mime, got nil")
	}
	if got := b.Status(); got != agent.StatusIdle {
		t.Fatalf("status after bad attachment: got %s, want %s", got, agent.StatusIdle)
	}
}

// TestConnectionClosedWhileIdle_MarksDead reproduces the root cause of the
// "needs attention" wedge a user hit by cancelling a turn almost instantly.
//
// Sequence: a turn settles to Idle, then the CLI subprocess/transport drops
// (the documented fallout of interrupting mid-enqueue — the subprocess exits).
// receiveLoop only promoted a dropped connection to StatusDead when the prior
// status was Busy/Starting, so an *Idle* session whose transport closed stayed
// "Idle" — a lie: the connection is gone. The next Send then dispatches into the
// dead transport, silently flips the session to StatusError, and (because the
// backend lingers in the host registry) never recovers.
//
// A closed connection means the backend is unusable regardless of the last
// turn's status; it MUST be marked Dead so the host can rehydrate it on the next
// op. Companion to TestClaudeCodeBackendConnectionClosed, which covers the Busy
// case that already worked.
func TestConnectionClosedWhileIdle_MarksDead(t *testing.T) {
	t.Parallel()

	const sessionID = "claude-idle-then-die"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		&claudecode.ResultMessage{MessageType: "result", SessionID: sessionID},
	})
	b := newTestBackend(t, transport)
	defer b.Stop()

	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{Text: "hi"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	// The turn completes and settles to Idle.
	waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// The transport drops while the session is Idle — e.g. the CLI exited as
	// fallout from an instant interrupt. This is NOT an intentional Stop().
	transport.Close()

	// The session must be marked Dead, not left looking like a usable Idle.
	waitForStatus(t, b.Events(), agent.StatusDead, 5*time.Second)
	if got := b.Status(); got != agent.StatusDead {
		t.Fatalf("status after transport closed while idle: got %s, want %s", got, agent.StatusDead)
	}
}

// TestSendDispatchFailure_EmitsErrorReason pins the recoverable-error contract:
// when a follow-up Send fails to dispatch (the CLI transport is dead), the
// backend must emit an error event carrying a reason — not flip to StatusError
// silently. The user-visible bug was a red status with no explanation and the
// typed text bouncing back, because the dispatch-failure path called
// setStatus(StatusError) without emitting an error reason for the client to show.
func TestSendDispatchFailure_EmitsErrorReason(t *testing.T) {
	t.Parallel()

	const sessionID = "claude-send-dispatch-fail"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		&claudecode.ResultMessage{MessageType: "result", SessionID: sessionID},
	})
	b := newTestBackend(t, transport)
	defer b.Stop()

	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{Text: "hi"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// The transport's write path is now dead (CLI subprocess gone).
	transport.mu.Lock()
	transport.sendErr = errors.New("write: broken pipe")
	transport.mu.Unlock()

	// The follow-up send fails to dispatch.
	if err := b.Send(context.Background(), agent.SendMessageOpts{Text: "still there?"}); err == nil {
		t.Fatal("expected Send to return an error when dispatch fails")
	}

	// The failure must surface as an error event with a non-empty reason so the
	// client can show a recoverable banner instead of a silent red status.
	evt := waitForEventType(t, b.Events(), agent.EventError, 2*time.Second)
	data, ok := evt.Data.(agent.ErrorData)
	if !ok {
		t.Fatalf("EventError carried %T, want agent.ErrorData", evt.Data)
	}
	if data.Message == "" {
		t.Error("EventError reason is empty; the client has nothing to show in the banner")
	}
	if got := b.Status(); got != agent.StatusError {
		t.Errorf("status after failed dispatch: got %s, want %s", got, agent.StatusError)
	}
}

// TestSelfInitiatedTurn_StreamFlipsBusyThenIdle pins the background-task
// status contract. When the agent spawns a background task (run_in_background
// Bash/Agent, Workflow), its turn ends immediately — result → StatusIdle —
// and the CLI later re-invokes the model ON ITS OWN when the task completes.
// No Send precedes that follow-up turn, and setStatus dedupes the terminating
// idle→idle, so before the markModelActive hook the entire re-invoked turn
// streamed with ZERO status frames: clients showed no spinner and no Stop
// button, and kept Send enabled against a working agent.
//
// The first message_start of the self-initiated turn must flip idle → Busy,
// and its result must settle Busy → Idle — real transitions on the wire for
// both edges.
func TestSelfInitiatedTurn_StreamFlipsBusyThenIdle(t *testing.T) {
	t.Parallel()

	const sessionID = "claude-bg-reinvoke-stream"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		// Turn 1 ends the moment the background task is spawned.
		&claudecode.ResultMessage{MessageType: "result", SessionID: sessionID},
		// Background task finished — the harness re-invokes the model.
		// message_start is the first wire signal; nothing else announces it.
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":    "message_start",
				"message": map[string]any{"id": "msg_reinvoke"},
			},
		},
		&claudecode.ResultMessage{MessageType: "result", SessionID: sessionID},
	})
	b := newTestBackend(t, transport)
	defer b.Stop()

	ch := b.Events()
	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{Text: "run the tests in the background"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	// Turn 1 settles while the background task runs.
	waitForStatus(t, ch, agent.StatusIdle, 5*time.Second)

	// The self-initiated turn must surface as a genuine idle→busy edge…
	events := waitForStatus(t, ch, agent.StatusBusy, 5*time.Second)
	last := events[len(events)-1]
	if d, ok := last.Data.(agent.StatusChangeData); !ok || d.OldStatus != agent.StatusIdle {
		t.Fatalf("busy transition should come from idle, got %+v", last.Data)
	}
	// …and settle back to idle when its result lands.
	waitForStatus(t, ch, agent.StatusIdle, 5*time.Second)
	if got := b.Status(); got != agent.StatusIdle {
		t.Fatalf("status after self-initiated turn: got %s, want %s", got, agent.StatusIdle)
	}
}

// TestSelfInitiatedTurn_AssistantMessageFlipsBusy is the no-partial-streaming
// variant of the test above: if stream events are unavailable, the full
// AssistantMessage snapshot is the first assistant-origin signal and must
// flip idle → Busy itself.
func TestSelfInitiatedTurn_AssistantMessageFlipsBusy(t *testing.T) {
	t.Parallel()

	const sessionID = "claude-bg-reinvoke-snapshot"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		&claudecode.ResultMessage{MessageType: "result", SessionID: sessionID},
		// Self-initiated turn delivered as a full snapshot, no stream events.
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.TextBlock{
					MessageType: "text",
					Text:        "The background tests finished — all green.",
				},
			},
		},
		&claudecode.ResultMessage{MessageType: "result", SessionID: sessionID},
	})
	b := newTestBackend(t, transport)
	defer b.Stop()

	ch := b.Events()
	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{Text: "run the tests in the background"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	waitForStatus(t, ch, agent.StatusIdle, 5*time.Second)

	events := waitForStatus(t, ch, agent.StatusBusy, 5*time.Second)
	last := events[len(events)-1]
	if d, ok := last.Data.(agent.StatusChangeData); !ok || d.OldStatus != agent.StatusIdle {
		t.Fatalf("busy transition should come from idle, got %+v", last.Data)
	}
	waitForStatus(t, ch, agent.StatusIdle, 5*time.Second)
}
