package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// TestClaudeCodeHelper is the subprocess that simulates claude CLI output.
// It's invoked by exec.Command with TEST_CLAUDE_HELPER=1.
func TestClaudeCodeHelper(t *testing.T) {
	if os.Getenv("TEST_CLAUDE_HELPER") != "1" {
		return
	}

	scenario := os.Getenv("TEST_CLAUDE_SCENARIO")

	switch scenario {
	case "basic":
		// Simulate: init → assistant message → result
		writeJSON(os.Stdout, map[string]interface{}{
			"type":       "system",
			"subtype":    "init",
			"session_id": "claude-session-abc123",
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type": "assistant",
			"message": map[string]interface{}{
				"id":   "msg-1",
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "I'll fix the bug now."},
				},
			},
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type":           "result",
			"subtype":        "success",
			"result":         "Bug fixed successfully",
			"session_id":     "claude-session-abc123",
			"total_cost_usd": 0.05,
			"is_error":       false,
		})

	case "tool_use":
		// Simulate: init → tool use → tool result → text → result
		writeJSON(os.Stdout, map[string]interface{}{
			"type":       "system",
			"subtype":    "init",
			"session_id": "claude-session-tools",
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type": "assistant",
			"message": map[string]interface{}{
				"id":   "msg-1",
				"role": "assistant",
				"content": []map[string]interface{}{
					{
						"type": "text",
						"text": "Let me read the file first.",
					},
					{
						"type":  "tool_use",
						"id":    "tool-1",
						"name":  "Read",
						"input": map[string]string{"path": "main.go"},
					},
				},
			},
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type": "assistant",
			"message": map[string]interface{}{
				"id":   "msg-2",
				"role": "assistant",
				"content": []map[string]interface{}{
					{
						"type":        "tool_result",
						"tool_use_id": "tool-1",
						"content":     "package main\nfunc main() {}",
					},
				},
			},
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type":     "result",
			"result":   "Done",
			"is_error": false,
		})

	case "error_result":
		// Simulate: init → error result
		writeJSON(os.Stdout, map[string]interface{}{
			"type":       "system",
			"subtype":    "init",
			"session_id": "claude-session-err",
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type":     "result",
			"result":   "API rate limit exceeded",
			"is_error": true,
		})

	case "streaming_deltas":
		// Simulate: init → content_block_start → content_block_delta → content_block_stop → result
		writeJSON(os.Stdout, map[string]interface{}{
			"type":       "system",
			"subtype":    "init",
			"session_id": "claude-session-stream",
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]interface{}{
				"type": "text",
				"text": "",
			},
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]interface{}{
				"type": "text_delta",
				"text": "Hello ",
			},
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]interface{}{
				"type": "text_delta",
				"text": "World!",
			},
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type":     "result",
			"result":   "Completed",
			"is_error": false,
		})

	case "thinking":
		// Simulate: init → thinking content → text → result
		writeJSON(os.Stdout, map[string]interface{}{
			"type":       "system",
			"subtype":    "init",
			"session_id": "claude-session-think",
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type": "assistant",
			"message": map[string]interface{}{
				"id":   "msg-1",
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "thinking", "text": "Let me think about this..."},
					{"type": "text", "text": "Here is my answer."},
				},
			},
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type":     "result",
			"result":   "Done",
			"is_error": false,
		})

	case "crash":
		// Simulate: init → crash (exit without result)
		writeJSON(os.Stdout, map[string]interface{}{
			"type":       "system",
			"subtype":    "init",
			"session_id": "claude-session-crash",
		})
		os.Exit(1)

	case "resume":
		// Simulate: resumed session
		writeJSON(os.Stdout, map[string]interface{}{
			"type":       "system",
			"subtype":    "init",
			"session_id": "existing-session-id",
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type": "assistant",
			"message": map[string]interface{}{
				"id":   "msg-1",
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Continuing from where we left off."},
				},
			},
		})
		writeJSON(os.Stdout, map[string]interface{}{
			"type":     "result",
			"result":   "Resumed successfully",
			"is_error": false,
		})

	default:
		fmt.Fprintf(os.Stderr, "unknown scenario: %s\n", scenario)
		os.Exit(2)
	}

	os.Exit(0)
}

func writeJSON(w *os.File, v interface{}) {
	data, _ := json.Marshal(v)
	w.Write(data)
	w.Write([]byte("\n"))
}

// helperCmdFactory creates a CmdFactory that runs this test binary as a
// subprocess with the given scenario.
func helperCmdFactory(scenario string) func(ctx context.Context, args []string, dir string) *exec.Cmd {
	return func(ctx context.Context, args []string, dir string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestClaudeCodeHelper")
		cmd.Env = append(os.Environ(),
			"TEST_CLAUDE_HELPER=1",
			"TEST_CLAUDE_SCENARIO="+scenario,
		)
		return cmd
	}
}

// --- Tests ---

func TestClaudeCodeBackendBasicSession(t *testing.T) {
	b := agent.NewClaudeCodeBackend()
	b.CmdFactory = helperCmdFactory("basic")
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/test",
		Prompt:     "Fix the bug",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Collect events. Expect:
	// 1. StatusChange starting->busy
	// 2. Message (assistant)
	// 3. PartUpdate (text)
	// 4. StatusChange busy->idle (from result)
	// 5. Message (result)
	events := collectEvents(b.Events(), 5, 5*time.Second)

	if len(events) < 5 {
		for i, e := range events {
			t.Logf("event %d: type=%s data=%+v", i, e.Type, e.Data)
		}
		t.Fatalf("expected at least 5 events, got %d", len(events))
	}

	// Check session ID was set.
	if b.SessionID() != "claude-session-abc123" {
		t.Errorf("expected session ID=claude-session-abc123, got %s", b.SessionID())
	}

	// First event: starting -> busy.
	if events[0].Type != agent.EventStatusChange {
		t.Errorf("event 0: expected status change, got %s", events[0].Type)
	}

	// Should have a message event for the assistant.
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

	// Should have text part update.
	var foundText bool
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Type == agent.PartText && data.Part.Text == "I'll fix the bug now." {
					foundText = true
				}
			}
		}
	}
	if !foundText {
		t.Error("expected text part update with 'I'll fix the bug now.'")
	}

	// Final status should be idle.
	// Give a moment for the process to fully exit and status to update.
	time.Sleep(100 * time.Millisecond)
	status := b.Status()
	if status != agent.StatusIdle && status != agent.StatusDead {
		t.Errorf("expected final status idle or dead, got %s", status)
	}
}

func TestClaudeCodeBackendToolUse(t *testing.T) {
	b := agent.NewClaudeCodeBackend()
	b.CmdFactory = helperCmdFactory("tool_use")
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/test",
		Prompt:     "Read and fix",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := collectEvents(b.Events(), 20, 3*time.Second)

	// Find tool call part.
	var foundToolCall bool
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Type == agent.PartToolCall && data.Part.Tool == "Read" {
					foundToolCall = true
				}
			}
		}
	}
	if !foundToolCall {
		t.Error("expected a tool_call part for 'Read'")
	}

	// Find tool result part.
	var foundToolResult bool
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Type == agent.PartToolResult {
					foundToolResult = true
				}
			}
		}
	}
	if !foundToolResult {
		t.Error("expected a tool_result part")
	}
}

func TestClaudeCodeBackendErrorResult(t *testing.T) {
	b := agent.NewClaudeCodeBackend()
	b.CmdFactory = helperCmdFactory("error_result")
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := collectEvents(b.Events(), 4, 5*time.Second)

	// Should have an error event.
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

	time.Sleep(100 * time.Millisecond)
	if b.Status() != agent.StatusError && b.Status() != agent.StatusDead {
		t.Errorf("expected error or dead status, got %s", b.Status())
	}
}

func TestClaudeCodeBackendStreamingDeltas(t *testing.T) {
	b := agent.NewClaudeCodeBackend()
	b.CmdFactory = helperCmdFactory("streaming_deltas")
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	events := collectEvents(b.Events(), 7, 5*time.Second)

	// Find text deltas.
	var deltas []string
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Type == agent.PartText && data.Part.Text != "" {
					deltas = append(deltas, data.Part.Text)
				}
			}
		}
	}

	// We should have the two deltas "Hello " and "World!" plus possibly
	// the initial empty block start.
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
	b := agent.NewClaudeCodeBackend()
	b.CmdFactory = helperCmdFactory("thinking")
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Events: starting->busy, message, thinking part, text part, busy->idle, result message
	events := collectEvents(b.Events(), 10, 3*time.Second)

	// Find thinking part.
	var foundThinking bool
	for _, evt := range events {
		if evt.Type == agent.EventPartUpdate {
			if data, ok := evt.Data.(agent.PartUpdateData); ok {
				if data.Part.Type == agent.PartThinking {
					foundThinking = true
					if data.Part.Text != "Let me think about this..." {
						t.Errorf("unexpected thinking text: %s", data.Part.Text)
					}
				}
			}
		}
	}
	if !foundThinking {
		t.Error("expected a thinking part update")
	}
}

func TestClaudeCodeBackendCrash(t *testing.T) {
	b := agent.NewClaudeCodeBackend()
	b.CmdFactory = helperCmdFactory("crash")
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for events channel to close.
	events := collectEvents(b.Events(), 10, 5*time.Second)

	// Should eventually get a dead status.
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
		t.Error("expected a status change to dead after crash")
	}
}

func TestClaudeCodeBackendResume(t *testing.T) {
	b := agent.NewClaudeCodeBackend()
	b.CmdFactory = helperCmdFactory("resume")
	defer b.Stop()

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/test",
		Prompt:     "Continue the work",
		SessionID:  "existing-session-id",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Events: starting->busy, message, text part, busy->idle, result message
	events := collectEvents(b.Events(), 10, 3*time.Second)
	if b.SessionID() != "existing-session-id" {
		t.Errorf("expected session ID=existing-session-id, got %s", b.SessionID())
	}

	// Should have text about continuing.
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

func TestClaudeCodeBackendSendMessageErrors(t *testing.T) {
	b := agent.NewClaudeCodeBackend()

	// Before start.
	err := b.SendMessage(context.Background(), agent.SendMessageOpts{Text: "hello"})
	if err == nil {
		t.Error("expected error sending message before Start")
	}
}

func TestClaudeCodeBackendAbortBeforeStart(t *testing.T) {
	b := agent.NewClaudeCodeBackend()

	err := b.Abort(context.Background())
	if err == nil {
		t.Error("expected error aborting before Start")
	}
}

func TestClaudeCodeBackendStopClosesEvents(t *testing.T) {
	b := agent.NewClaudeCodeBackend()
	b.CmdFactory = helperCmdFactory("basic")

	err := b.Start(context.Background(), agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Let it run a bit, then stop.
	time.Sleep(100 * time.Millisecond)
	b.Stop()

	// Events channel should close.
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
