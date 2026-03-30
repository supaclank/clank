package agent_test

import (
	"context"
	"sync"
	"testing"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"

	"github.com/acksell/clank/internal/agent"
)

// mockTransport implements claudecode.Transport for testing. It feeds
// pre-scripted Message sequences into the receive channel and records
// prompts sent via SendMessage.
type mockTransport struct {
	mu       sync.Mutex
	messages []claudecode.Message // Messages to deliver on ReceiveMessages

	// interrupted is set to true when Interrupt is called.
	interrupted bool

	// connected tracks connection state.
	connected bool
	closed    bool

	// msgChan and errChan are created on Connect and fed by a goroutine.
	msgChan chan claudecode.Message
	errChan chan error

	// onSend is called each time SendMessage is invoked. If it returns
	// additional messages, they are delivered on the message channel.
	// This enables simulating follow-up responses.
	onSend func(prompt string) []claudecode.Message
}

func newMockTransport(messages []claudecode.Message) *mockTransport {
	return &mockTransport{messages: messages}
}

func (t *mockTransport) Connect(_ context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.connected = true
	t.closed = false
	t.msgChan = make(chan claudecode.Message, 128)
	t.errChan = make(chan error, 1)

	// Deliver initial messages in a goroutine to avoid blocking.
	msgs := make([]claudecode.Message, len(t.messages))
	copy(msgs, t.messages)

	go func() {
		for _, m := range msgs {
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if closed {
				return
			}
			t.msgChan <- m
		}
	}()

	return nil
}

func (t *mockTransport) SendMessage(_ context.Context, msg claudecode.StreamMessage) error {
	t.mu.Lock()
	onSend := t.onSend
	t.mu.Unlock()

	// If the test registered an onSend callback, deliver follow-up messages.
	if onSend != nil {
		// Extract the prompt text from the StreamMessage.
		prompt := ""
		if m, ok := msg.Message.(map[string]interface{}); ok {
			if c, ok := m["content"].(string); ok {
				prompt = c
			}
		}
		followUp := onSend(prompt)
		for _, m := range followUp {
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if closed {
				return nil
			}
			t.msgChan <- m
		}
	}

	return nil
}

func (t *mockTransport) ReceiveMessages(_ context.Context) (<-chan claudecode.Message, <-chan error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.msgChan, t.errChan
}

func (t *mockTransport) Interrupt(_ context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.interrupted = true
	return nil
}

func (t *mockTransport) SetModel(_ context.Context, _ *string) error { return nil }
func (t *mockTransport) SetPermissionMode(_ context.Context, _ string) error {
	return nil
}
func (t *mockTransport) RewindFiles(_ context.Context, _ string) error { return nil }

func (t *mockTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.connected && !t.closed && t.msgChan != nil {
		t.closed = true
		close(t.msgChan)
	}
	t.connected = false
	return nil
}

func (t *mockTransport) GetValidator() *claudecode.StreamValidator { return nil }

// --- Test helpers ---

// newTestBackend creates a ClaudeCodeBackend wired to the given mock transport.
// The ClientFactory bypasses CLI discovery by using NewClientWithTransport.
func newTestBackend(transport *mockTransport) *agent.ClaudeCodeBackend {
	b := agent.NewClaudeCodeBackend()
	b.ClientFactory = func(opts ...claudecode.Option) claudecode.Client {
		return claudecode.NewClientWithTransport(transport, opts...)
	}
	return b
}

// waitForStatus drains the events channel until the target status is observed.
func waitForStatus(t *testing.T, ch <-chan agent.Event, target agent.SessionStatus, timeout time.Duration) []agent.Event {
	t.Helper()
	var events []agent.Event
	timer := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				t.Fatalf("events channel closed before status %s", target)
			}
			events = append(events, evt)
			if evt.Type == agent.EventStatusChange {
				if data, ok := evt.Data.(agent.StatusChangeData); ok {
					if data.NewStatus == target {
						return events
					}
				}
			}
		case <-timer:
			t.Fatalf("timed out waiting for status %s (got %d events)", target, len(events))
		}
	}
}

// --- Tests ---

func TestClaudeCodeBackendBasicSession(t *testing.T) {
	t.Parallel()

	sessionID := "claude-session-abc123"
	result := "Bug fixed successfully"

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		// message_start carries the Anthropic message ID.
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":    "message_start",
				"message": map[string]any{"id": "msg_basic_001"},
			},
		},
		// Streaming deltas arrive before AssistantMessage.
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "I'll fix the bug now.",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_stop",
				"index": float64(0),
			},
		},
		// AssistantMessage is the final snapshot — only used for Messages() accumulation.
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.TextBlock{
					MessageType: "text",
					Text:        "I'll fix the bug now.",
				},
			},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   sessionID,
			Result:      &result,
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "Fix the bug",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for idle (result delivered).
	events := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Verify session ID.
	if b.SessionID() != sessionID {
		t.Errorf("expected session ID=%s, got %s", sessionID, b.SessionID())
	}

	// First event: starting -> busy.
	if events[0].Type != agent.EventStatusChange {
		t.Errorf("event 0: expected status change, got %s", events[0].Type)
	}

	// Should have an assistant message event (content-less shell).
	var foundMsg bool
	for _, evt := range events {
		if evt.Type == agent.EventMessage {
			if data, ok := evt.Data.(agent.MessageData); ok {
				if data.Role == "assistant" {
					foundMsg = true
				}
			}
		}
	}
	if !foundMsg {
		t.Error("expected an assistant message event")
	}

	// Should have text part update from streaming with message-scoped ID.
	var foundText bool
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Type == agent.PartText && data.Part.Text == "I'll fix the bug now." {
					foundText = true
					// Verify the part ID uses the message ID, not a counter.
					if data.Part.ID != "msg_basic_001-0" {
						t.Errorf("expected part ID 'msg_basic_001-0', got %q", data.Part.ID)
					}
				}
			}
		}
	}
	if !foundText {
		t.Error("expected text part update with 'I'll fix the bug now.'")
	}

	// Verify no duplicate result message.
	var msgCount int
	for _, evt := range events {
		if evt.Type == agent.EventMessage {
			msgCount++
		}
	}
	if msgCount != 1 {
		t.Errorf("expected exactly 1 message event (no result duplicate), got %d", msgCount)
	}
}

func TestClaudeCodeBackendToolUse(t *testing.T) {
	t.Parallel()

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": "claude-session-tools"},
		},
		// Streaming: tool use block starts.
		&claudecode.StreamEvent{
			SessionID: "claude-session-tools",
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: "claude-session-tools",
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "Let me read the file first.",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: "claude-session-tools",
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(1),
				"content_block": map[string]any{
					"type": "tool_use",
					"id":   "tool-1",
					"name": "Read",
				},
			},
		},
		// AssistantMessage snapshot (used for Messages() only).
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.TextBlock{
					MessageType: "text",
					Text:        "Let me read the file first.",
				},
				&claudecode.ToolUseBlock{
					MessageType: "tool_use",
					ToolUseID:   "tool-1",
					Name:        "Read",
					Input:       map[string]any{"path": "main.go"},
				},
			},
		},
		// Tool result arrives as a separate AssistantMessage.
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.ToolResultBlock{
					MessageType: "tool_result",
					ToolUseID:   "tool-1",
					Content:     "package main\nfunc main() {}",
				},
			},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   "claude-session-tools",
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "Read and fix",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Find tool call part from streaming.
	var foundToolCall bool
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Type == agent.PartToolCall && data.Part.Tool == "Read" {
					foundToolCall = true
					if data.Part.Status != agent.PartRunning {
						t.Errorf("tool call status: expected running, got %s", data.Part.Status)
					}
				}
			}
		}
	}
	if !foundToolCall {
		t.Error("expected a tool_call part for 'Read'")
	}

	// Verify tool result is accumulated in Messages().
	msgs, err := b.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	var foundToolResult bool
	for _, msg := range msgs {
		for _, p := range msg.Parts {
			if p.Type == agent.PartToolResult && p.Status == agent.PartCompleted {
				foundToolResult = true
			}
		}
	}
	if !foundToolResult {
		t.Error("expected a tool_result part in Messages()")
	}
}

func TestClaudeCodeBackendErrorResult(t *testing.T) {
	t.Parallel()

	errMsg := "API rate limit exceeded"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": "claude-session-err"},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   "claude-session-err",
			IsError:     true,
			Result:      &errMsg,
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := waitForStatus(t, b.Events(), agent.StatusError, 5*time.Second)

	var foundError bool
	for _, evt := range events {
		if evt.Type == agent.EventError {
			if data, ok := evt.Data.(agent.ErrorData); ok {
				if data.Message == "API rate limit exceeded" {
					foundError = true
				}
			}
		}
	}
	if !foundError {
		t.Error("expected error event with 'API rate limit exceeded'")
	}
}

func TestClaudeCodeBackendStreamingDeltas(t *testing.T) {
	t.Parallel()

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": "claude-session-stream"},
		},
		&claudecode.StreamEvent{
			SessionID: "claude-session-stream",
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: "claude-session-stream",
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "Hello ",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: "claude-session-stream",
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "World!",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: "claude-session-stream",
			Event: map[string]any{
				"type":  "content_block_stop",
				"index": float64(0),
			},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   "claude-session-stream",
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Find text deltas.
	var deltas []string
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Type == agent.PartText && data.IsDelta && data.Part.Text != "" {
					deltas = append(deltas, data.Part.Text)
				}
			}
		}
	}

	foundHello := false
	foundWorld := false
	for _, d := range deltas {
		if d == "Hello " {
			foundHello = true
		}
		if d == "World!" {
			foundWorld = true
		}
	}
	if !foundHello || !foundWorld {
		t.Errorf("expected deltas 'Hello ' and 'World!', got %v", deltas)
	}
}

func TestClaudeCodeBackendThinking(t *testing.T) {
	t.Parallel()

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": "claude-session-think"},
		},
		// Streaming: thinking block.
		&claudecode.StreamEvent{
			SessionID: "claude-session-think",
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type":     "thinking",
					"thinking": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: "claude-session-think",
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": "Let me think about this...",
				},
			},
		},
		// Streaming: text block.
		&claudecode.StreamEvent{
			SessionID: "claude-session-think",
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(1),
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: "claude-session-think",
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(1),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "Here is my answer.",
				},
			},
		},
		// AssistantMessage snapshot (for Messages() accumulation only).
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.ThinkingBlock{
					MessageType: "thinking",
					Thinking:    "Let me think about this...",
				},
				&claudecode.TextBlock{
					MessageType: "text",
					Text:        "Here is my answer.",
				},
			},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   "claude-session-think",
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	var foundThinking bool
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Type == agent.PartThinking && data.IsDelta && data.Part.Text == "Let me think about this..." {
					foundThinking = true
				}
			}
		}
	}
	if !foundThinking {
		t.Error("expected a thinking delta part update with 'Let me think about this...'")
	}
}

func TestClaudeCodeBackendConnectionClosed(t *testing.T) {
	t.Parallel()

	// Simulate: init message, then connection closes abruptly (no result).
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": "claude-session-crash"},
		},
	})

	b := newTestBackend(transport)

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Close the transport to simulate a crash — the message channel closes.
	transport.Close()

	events := waitForStatus(t, b.Events(), agent.StatusDead, 5*time.Second)

	var foundDead bool
	for _, evt := range events {
		if evt.Type == agent.EventStatusChange {
			if data, ok := evt.Data.(agent.StatusChangeData); ok {
				if data.NewStatus == agent.StatusDead {
					foundDead = true
				}
			}
		}
	}
	if !foundDead {
		t.Error("expected a status change to dead after connection close")
	}

	// Now stop (should not panic on already-closed channel).
	b.Stop()
}

func TestClaudeCodeBackendResume(t *testing.T) {
	t.Parallel()

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": "existing-session-id"},
		},
		// Streaming deltas.
		&claudecode.StreamEvent{
			SessionID: "existing-session-id",
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: "existing-session-id",
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "Continuing from where we left off.",
				},
			},
		},
		// AssistantMessage snapshot.
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.TextBlock{
					MessageType: "text",
					Text:        "Continuing from where we left off.",
				},
			},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   "existing-session-id",
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "Continue the work",
		SessionID:  "existing-session-id",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	if b.SessionID() != "existing-session-id" {
		t.Errorf("expected session ID=existing-session-id, got %s", b.SessionID())
	}

	var foundContinue bool
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Text == "Continuing from where we left off." {
					foundContinue = true
				}
			}
		}
	}
	if !foundContinue {
		t.Error("expected text part with 'Continuing from where we left off.'")
	}
}

func TestClaudeCodeBackendSendMessageBeforeStart(t *testing.T) {
	t.Parallel()
	b := agent.NewClaudeCodeBackend()
	defer b.Stop()

	err := b.SendMessage(context.Background(), agent.SendMessageOpts{Text: "hello"})
	if err == nil {
		t.Error("expected error sending message before Start")
	}
}

func TestClaudeCodeBackendSendMessageFollowUp(t *testing.T) {
	t.Parallel()

	sessionID := "claude-session-abc123"
	initResult := "Bug fixed"
	followUpResult := "Follow-up done"

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		// First turn: streaming deltas + snapshot.
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "I'll fix the bug now.",
				},
			},
		},
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.TextBlock{MessageType: "text", Text: "I'll fix the bug now."},
			},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   sessionID,
			Result:      &initResult,
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "Fix the bug",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the first turn to complete.
	waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Set up onSend AFTER the first turn to avoid firing on the initial Query().
	transport.onSend = func(prompt string) []claudecode.Message {
		return []claudecode.Message{
			// Streaming deltas for follow-up.
			&claudecode.StreamEvent{
				SessionID: sessionID,
				Event: map[string]any{
					"type":  "content_block_start",
					"index": float64(0),
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				},
			},
			&claudecode.StreamEvent{
				SessionID: sessionID,
				Event: map[string]any{
					"type":  "content_block_delta",
					"index": float64(0),
					"delta": map[string]any{
						"type": "text_delta",
						"text": "Here is the follow-up response.",
					},
				},
			},
			// Snapshot.
			&claudecode.AssistantMessage{
				MessageType: "assistant",
				Content: []claudecode.ContentBlock{
					&claudecode.TextBlock{MessageType: "text", Text: "Here is the follow-up response."},
				},
			},
			&claudecode.ResultMessage{
				MessageType: "result",
				SessionID:   sessionID,
				Result:      &followUpResult,
			},
		}
	}

	if b.SessionID() != sessionID {
		t.Fatalf("expected session ID=%s, got %s", sessionID, b.SessionID())
	}

	// Send follow-up.
	err = b.SendMessage(context.Background(), agent.SendMessageOpts{Text: "What about the other bug?"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Wait for second turn to complete.
	followUpEvents := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Verify we got the follow-up response text.
	var foundFollowUp bool
	for _, evt := range followUpEvents {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Text == "Here is the follow-up response." {
					foundFollowUp = true
				}
			}
		}
	}
	if !foundFollowUp {
		for i, e := range followUpEvents {
			t.Logf("follow-up event %d: type=%s data=%+v", i, e.Type, e.Data)
		}
		t.Error("expected text part with 'Here is the follow-up response.' from follow-up")
	}

	// Verify the user's follow-up prompt was emitted as a user message event.
	var foundUserMsg bool
	for _, evt := range followUpEvents {
		if evt.Type == agent.EventMessage {
			if data, ok := evt.Data.(agent.MessageData); ok {
				if data.Role == "user" && data.Content == "What about the other bug?" {
					foundUserMsg = true
				}
			}
		}
	}
	if !foundUserMsg {
		t.Error("expected user message event for follow-up prompt")
	}

	// Session ID should remain the same.
	if b.SessionID() != sessionID {
		t.Errorf("session ID changed after follow-up: got %s", b.SessionID())
	}
}

func TestClaudeCodeBackendAbortBeforeStart(t *testing.T) {
	t.Parallel()
	b := agent.NewClaudeCodeBackend()
	defer b.Stop()

	err := b.Abort(context.Background())
	if err == nil {
		t.Error("expected error aborting before Start")
	}
}

func TestClaudeCodeBackendAbortCallsInterrupt(t *testing.T) {
	t.Parallel()

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": "session-abort"},
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give receive loop time to start.
	time.Sleep(50 * time.Millisecond)

	err = b.Abort(context.Background())
	if err != nil {
		t.Errorf("Abort: %v", err)
	}

	transport.mu.Lock()
	interrupted := transport.interrupted
	transport.mu.Unlock()

	if !interrupted {
		t.Error("expected transport.Interrupt to be called")
	}
}

func TestClaudeCodeBackendStopClosesEvents(t *testing.T) {
	t.Parallel()

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": "session-stop"},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   "session-stop",
		},
	})

	b := newTestBackend(transport)

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Let messages flow through.
	time.Sleep(100 * time.Millisecond)
	b.Stop()

	// Events channel should close after Stop().
	select {
	case _, ok := <-b.Events():
		if ok {
			// Drain remaining events.
			for range b.Events() {
			}
		}
		// Channel closed — good.
	case <-time.After(5 * time.Second):
		t.Error("events channel did not close after Stop")
	}
}

func TestClaudeCodeBackendNoDuplicateResultMessage(t *testing.T) {
	t.Parallel()

	result := "Completed"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": "claude-session-stream"},
		},
		// Streaming deltas (partial messages).
		&claudecode.StreamEvent{
			SessionID: "claude-session-stream",
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "Hello World!",
				},
			},
		},
		// Result arrives.
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   "claude-session-stream",
			Result:      &result,
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Verify no EventMessage contains the result text.
	for _, evt := range events {
		if evt.Type == agent.EventMessage {
			if data, ok := evt.Data.(agent.MessageData); ok {
				if data.Content == "Completed" {
					t.Error("result text 'Completed' should not appear as an EventMessage — it duplicates streamed content")
				}
			}
		}
	}
}

// TestClaudeCodeBackendNoDuplicateContent verifies that streaming deltas
// and AssistantMessage don't produce duplicate entries. The AssistantMessage
// should emit a content-less EventMessage shell (like OpenCode), not per-part
// EventPartUpdate events.
func TestClaudeCodeBackendNoDuplicateContent(t *testing.T) {
	t.Parallel()

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": "session-nodup"},
		},
		// Streaming delivers the text.
		&claudecode.StreamEvent{
			SessionID: "session-nodup",
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: "session-nodup",
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "Hello!",
				},
			},
		},
		// AssistantMessage snapshot (should NOT produce EventPartUpdate).
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.TextBlock{
					MessageType: "text",
					Text:        "Hello!",
				},
			},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   "session-nodup",
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Count text part updates — should have exactly 2:
	// 1 from content_block_start (empty text) and 1 from content_block_delta.
	// The AssistantMessage must NOT add a third.
	var textPartUpdates int
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Type == agent.PartText {
					textPartUpdates++
				}
			}
		}
	}
	if textPartUpdates != 2 {
		t.Errorf("expected exactly 2 text EventPartUpdate (start + delta), got %d", textPartUpdates)
		for i, evt := range events {
			t.Logf("event %d: type=%s data=%+v", i, evt.Type, evt.Data)
		}
	}

	// The EventMessage from AssistantMessage should be a content-less shell.
	for _, evt := range events {
		if evt.Type == agent.EventMessage {
			if data, ok := evt.Data.(agent.MessageData); ok {
				if data.Role == "assistant" && (data.Content != "" || len(data.Parts) > 0) {
					t.Errorf("AssistantMessage should emit content-less EventMessage shell, got content=%q parts=%d",
						data.Content, len(data.Parts))
				}
			}
		}
	}
}

func TestClaudeCodeBackendMessagesAccumulation(t *testing.T) {
	t.Parallel()

	sessionID := "session-msgs"
	result := "Done"

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		// Streaming + snapshot for first turn.
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "First response.",
				},
			},
		},
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.TextBlock{MessageType: "text", Text: "First response."},
			},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   sessionID,
			Result:      &result,
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "First prompt",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Set up onSend AFTER the first turn to avoid firing on the initial Query().
	followUpDone := "Follow-up done"
	transport.onSend = func(prompt string) []claudecode.Message {
		return []claudecode.Message{
			&claudecode.StreamEvent{
				SessionID: sessionID,
				Event: map[string]any{
					"type":  "content_block_delta",
					"index": float64(0),
					"delta": map[string]any{
						"type": "text_delta",
						"text": "Second response.",
					},
				},
			},
			&claudecode.AssistantMessage{
				MessageType: "assistant",
				Content: []claudecode.ContentBlock{
					&claudecode.TextBlock{MessageType: "text", Text: "Second response."},
				},
			},
			&claudecode.ResultMessage{
				MessageType: "result",
				SessionID:   sessionID,
				Result:      &followUpDone,
			},
		}
	}

	// Send follow-up.
	err = b.SendMessage(context.Background(), agent.SendMessageOpts{Text: "Second prompt"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Check Messages() returns accumulated history.
	msgs, err := b.Messages(context.Background())
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}

	// Expected: assistant (first), user (follow-up), assistant (second) = 3 messages.
	if len(msgs) != 3 {
		for i, m := range msgs {
			t.Logf("msg %d: role=%s content=%q parts=%d", i, m.Role, m.Content, len(m.Parts))
		}
		t.Fatalf("expected 3 messages in history, got %d", len(msgs))
	}

	if msgs[0].Role != "assistant" {
		t.Errorf("msg 0: expected role=assistant, got %s", msgs[0].Role)
	}
	if msgs[1].Role != "user" {
		t.Errorf("msg 1: expected role=user, got %s", msgs[1].Role)
	}
	if msgs[1].Content != "Second prompt" {
		t.Errorf("msg 1: expected content='Second prompt', got %q", msgs[1].Content)
	}
	if msgs[2].Role != "assistant" {
		t.Errorf("msg 2: expected role=assistant, got %s", msgs[2].Role)
	}
}

// TestClaudeCodeBackendMultiCyclePartIDs is a regression test for the bug where
// tool use within a single turn causes multiple message cycles, each with block
// indices resetting to 0. Without message-scoped IDs, the second cycle's
// "index=0" block collides with the first cycle's "index=0" block, causing the
// TUI to append text to the wrong entry or drop content entirely.
//
// This test simulates: thinking(0) + text(1) + tool_use(2) → tool result →
// new message cycle with text(0). The text in the second cycle must get a
// different part ID than the thinking in the first cycle, even though both
// are at block index 0.
func TestClaudeCodeBackendMultiCyclePartIDs(t *testing.T) {
	t.Parallel()

	sessionID := "session-multi-cycle"

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},

		// --- Message cycle 1: thinking + text + tool_use ---
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":    "message_start",
				"message": map[string]any{"id": "msg_cycle1"},
			},
		},
		// thinking at index 0
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type":     "thinking",
					"thinking": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type":     "thinking_delta",
					"thinking": "Let me read the file.",
				},
			},
		},
		// text at index 1
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(1),
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(1),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "I'll read the file.",
				},
			},
		},
		// tool_use at index 2 (has its own natural ID)
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(2),
				"content_block": map[string]any{
					"type": "tool_use",
					"id":   "toolu_001",
					"name": "Read",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "message_stop",
				"index": float64(0),
			},
		},
		// AssistantMessage snapshot for cycle 1
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.ThinkingBlock{MessageType: "thinking", Thinking: "Let me read the file."},
				&claudecode.TextBlock{MessageType: "text", Text: "I'll read the file."},
				&claudecode.ToolUseBlock{
					MessageType: "tool_use",
					ToolUseID:   "toolu_001",
					Name:        "Read",
					Input:       map[string]any{"path": "main.go"},
				},
			},
		},

		// --- Message cycle 2: text response after tool result ---
		// (block indices reset to 0, but message ID is different)
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":    "message_start",
				"message": map[string]any{"id": "msg_cycle2"},
			},
		},
		// text at index 0 — same index as thinking in cycle 1!
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "The file contains a main function.",
				},
			},
		},
		// Snapshot for cycle 2
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.TextBlock{MessageType: "text", Text: "The file contains a main function."},
			},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   sessionID,
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "Read the file",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Collect all part IDs from EventPartUpdate events.
	partIDs := make(map[string]agent.PartUpdateData)
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				// Only record the first occurrence (content_block_start or first delta).
				if _, exists := partIDs[data.Part.ID]; !exists {
					partIDs[data.Part.ID] = data
				}
			}
		}
	}

	// Verify: cycle 1's thinking (index=0) and cycle 2's text (index=0) must
	// have DIFFERENT part IDs despite both being at block index 0.
	cycle1ThinkingID := "msg_cycle1-0"
	cycle2TextID := "msg_cycle2-0"

	if _, ok := partIDs[cycle1ThinkingID]; !ok {
		t.Errorf("expected part with ID %q (cycle 1 thinking), got IDs: %v", cycle1ThinkingID, keys(partIDs))
	}
	if _, ok := partIDs[cycle2TextID]; !ok {
		t.Errorf("expected part with ID %q (cycle 2 text), got IDs: %v", cycle2TextID, keys(partIDs))
	}

	// They must be different IDs (this is the core invariant).
	if cycle1ThinkingID == cycle2TextID {
		t.Fatal("BUG: cycle 1 thinking and cycle 2 text have the same part ID — this causes the TUI to merge them")
	}

	// Verify cycle 1 thinking has the right content.
	if data, ok := partIDs[cycle1ThinkingID]; ok {
		if data.Part.Type != agent.PartThinking {
			t.Errorf("cycle 1 index=0: expected type=thinking, got %s", data.Part.Type)
		}
	}

	// Verify cycle 2 text has the right content.
	if data, ok := partIDs[cycle2TextID]; ok {
		if data.Part.Type != agent.PartText {
			t.Errorf("cycle 2 index=0: expected type=text, got %s", data.Part.Type)
		}
	}

	// Verify tool_use block uses its natural ToolUseID, not a message-scoped ID.
	if _, ok := partIDs["toolu_001"]; !ok {
		t.Errorf("expected tool_use part with ID 'toolu_001', got IDs: %v", keys(partIDs))
	}

	// Verify total unique part IDs: thinking, text(cycle1), tool_use, text(cycle2) = 4.
	// (Plus deltas that reuse the same IDs.)
	expectedIDs := map[string]bool{
		"msg_cycle1-0": true, // thinking
		"msg_cycle1-1": true, // text
		"toolu_001":    true, // tool_use
		"msg_cycle2-0": true, // text (after tool result)
	}
	for id := range expectedIDs {
		if _, ok := partIDs[id]; !ok {
			t.Errorf("missing expected part ID %q", id)
		}
	}
}

func keys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestClaudeCodeBackendToolCallSpinnerCompletion is a regression test for the
// "stuck spinner" bug. When a tool_use block finishes (content_block_stop),
// the backend must emit an EventPartUpdate with Status=PartCompleted so the
// TUI transitions from the spinning indicator to ✓. Without this, tool calls
// show a spinner indefinitely because the tool_result arrives in a separate
// message cycle and doesn't update the original tool_call part's status.
func TestClaudeCodeBackendToolCallSpinnerCompletion(t *testing.T) {
	t.Parallel()

	sessionID := "session-spinner"

	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		// message_start establishes the message scope.
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":    "message_start",
				"message": map[string]any{"id": "msg_spinner_001"},
			},
		},
		// text block at index 0
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type": "text_delta",
					"text": "Let me edit the file.",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_stop",
				"index": float64(0),
			},
		},
		// tool_use block at index 1
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(1),
				"content_block": map[string]any{
					"type": "tool_use",
					"id":   "toolu_spinner_001",
					"name": "Write",
				},
			},
		},
		// tool input arrives incrementally (we skip it, but it's realistic)
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(1),
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": `{"path":"main.go"}`,
				},
			},
		},
		// content_block_stop for the tool_use block — THIS must trigger PartCompleted.
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_stop",
				"index": float64(1),
			},
		},
		// AssistantMessage snapshot (for Messages() accumulation).
		&claudecode.AssistantMessage{
			MessageType: "assistant",
			Content: []claudecode.ContentBlock{
				&claudecode.TextBlock{
					MessageType: "text",
					Text:        "Let me edit the file.",
				},
				&claudecode.ToolUseBlock{
					MessageType: "tool_use",
					ToolUseID:   "toolu_spinner_001",
					Name:        "Write",
					Input:       map[string]any{"path": "main.go"},
				},
			},
		},
		&claudecode.ResultMessage{
			MessageType: "result",
			SessionID:   sessionID,
		},
	})

	b := newTestBackend(transport)
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: t.TempDir(),
		Prompt:     "Edit the file",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := waitForStatus(t, b.Events(), agent.StatusIdle, 5*time.Second)

	// Collect all EventPartUpdate events for the tool call.
	var toolCallRunning, toolCallCompleted bool
	for _, evt := range events {
		if evt.Type != agent.EventPartUpdate {
			continue
		}
		data, ok := evt.Data.(agent.PartUpdateData)
		if !ok {
			continue
		}
		if data.Part.ID != "toolu_spinner_001" {
			continue
		}
		if data.Part.Type == agent.PartToolCall && data.Part.Status == agent.PartRunning {
			toolCallRunning = true
		}
		if data.Part.Type == agent.PartToolCall && data.Part.Status == agent.PartCompleted {
			toolCallCompleted = true
		}
	}

	if !toolCallRunning {
		t.Error("expected a PartRunning event for tool call 'toolu_spinner_001'")
	}
	if !toolCallCompleted {
		t.Error("expected a PartCompleted event for tool call 'toolu_spinner_001' (spinner should stop)")
		t.Log("This is the 'stuck spinner' regression — content_block_stop must emit PartCompleted")
		for i, evt := range events {
			t.Logf("event %d: type=%s data=%+v", i, evt.Type, evt.Data)
		}
	}

	// Verify that the text block's content_block_stop does NOT emit a spurious
	// PartCompleted (only tool_use blocks are tracked in activeBlocks).
	for _, evt := range events {
		if evt.Type != agent.EventPartUpdate {
			continue
		}
		data, ok := evt.Data.(agent.PartUpdateData)
		if !ok {
			continue
		}
		if data.Part.ID == "msg_spinner_001-0" && data.Part.Status == agent.PartCompleted {
			t.Error("text block should NOT receive a PartCompleted event from content_block_stop")
		}
	}
}
