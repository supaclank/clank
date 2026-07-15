package agent_test

import (
	"context"
	"testing"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"

	"github.com/acksell/clank/internal/agent"
)

// TestClaudeSurfacesThinkingFromCompletedMessage is the regression test
// for the 2026-07-15 "no thinking, just tool calls with long gaps" UX
// bug. Extended-thinking blocks stream only a content_block_start plus a
// signature_delta — the thinking TEXT is NEVER sent as a thinking_delta,
// so the streaming path produces only an empty PartThinking. The full
// text arrives solely in the completed AssistantMessage, so
// handleAssistantMessage must surface it (keyed to the same
// {msgID}-{index}) or the user sees nothing between tool calls.
func TestClaudeSurfacesThinkingFromCompletedMessage(t *testing.T) {
	t.Parallel()

	const (
		sessionID    = "claude-thinking"
		apiMsgID     = "msg_think1"
		thinkingText = "The Monty Hall answer is 2/3: switching wins exactly when the first pick was wrong, which is 2 of 3 times."
	)

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		// message_start establishes currentMsgID so block IDs line up.
		&claudecode.StreamEvent{SessionID: sessionID, Event: map[string]any{
			"type":    "message_start",
			"message": map[string]any{"id": apiMsgID},
		}},
		// The thinking block streams start + signature only — NO
		// thinking_delta. This is the exact shape observed on the wire.
		&claudecode.StreamEvent{SessionID: sessionID, Event: map[string]any{
			"type":          "content_block_start",
			"index":         float64(0),
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		}},
		&claudecode.StreamEvent{SessionID: sessionID, Event: map[string]any{
			"type":  "content_block_delta",
			"index": float64(0),
			"delta": map[string]any{"type": "signature_delta", "signature": "sig-abc"},
		}},
		&claudecode.StreamEvent{SessionID: sessionID, Event: map[string]any{
			"type":  "content_block_stop",
			"index": float64(0),
		}},
		// The completed message is the only carrier of the thinking text.
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.ThinkingBlock{MessageType: "thinking", Thinking: thinkingText, Signature: "sig-abc"},
			},
		},
		&claudecode.ResultMessage{MessageType: "result", SessionID: sessionID},
	})

	b := newTestBackend(t, transport)
	defer b.Stop()

	if err := b.OpenAndSend(context.Background(), agent.SendMessageOpts{Text: "reason it out"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	events := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	var gotText, gotID string
	var found bool
	for _, evt := range events {
		if evt.Type != agent.EventPartUpdate {
			continue
		}
		d, ok := evt.Data.(agent.PartUpdateData)
		if !ok || d.Part.Type != agent.PartThinking {
			continue
		}
		if d.Part.Text != "" { // ignore the empty streamed shell
			found = true
			gotText = d.Part.Text
			gotID = d.Part.ID
		}
	}

	if !found {
		t.Fatal("no non-empty PartThinking surfaced — Sonnet's thinking is invisible between tool calls (the reported UX bug)")
	}
	if gotText != thinkingText {
		t.Errorf("thinking text = %q, want the full block text %q", gotText, thinkingText)
	}
	// Same ID scheme as the streaming path so it REPLACES the empty shell
	// rather than appending a duplicate thinking part.
	if want := apiMsgID + "-0"; gotID != want {
		t.Errorf("thinking part ID = %q, want %q (must match the streamed block's id)", gotID, want)
	}
}
