package agent_test

import (
	"context"
	"strings"
	"testing"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"

	"github.com/acksell/clank/internal/agent"
)

// askUserQuestionInput is a canonical AskUserQuestion tool input, as the CLI
// hands it to the CanUseTool callback.
func askUserQuestionInput() map[string]any {
	return map[string]any{
		"questions": []any{
			map[string]any{
				"question": "Which auth method should we use?",
				"header":   "Auth",
				"options": []any{
					map[string]any{"label": "JWT", "description": "Stateless tokens"},
					map[string]any{"label": "Sessions"},
				},
			},
		},
	}
}

// waitForQuestionPart drains events until a tool part carrying a question tag
// arrives, returning it.
func waitForQuestionPart(t *testing.T, ch <-chan agent.Event, timeout time.Duration) agent.Part {
	t.Helper()
	timer := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				t.Fatal("events channel closed before a tagged question part")
			}
			if evt.Type != agent.EventPartUpdate {
				continue
			}
			if d, ok := evt.Data.(agent.PartUpdateData); ok && d.Part.Question != nil {
				return d.Part
			}
		case <-timer:
			t.Fatal("timed out waiting for a tagged question part")
		}
	}
}

// A gated AskUserQuestion must surface as a tool part tagged with the
// normalized prompt (before the permission event, same request id), and
// RespondQuestion must deliver the formatted answers to the model as the
// permission deny reason — the only transport that reaches a parked prompt
// (allow would re-run the tool's interactive picker headless and deadlock).
func TestClaudeCodeBackend_Question_GatedFlow(t *testing.T) {
	t.Parallel()
	transport := newMockTransport([]claudecode.Message{
		&claudecode.StreamEvent{Event: map[string]any{
			"type":  "content_block_start",
			"index": float64(0),
			"content_block": map[string]any{
				"type": "tool_use",
				"id":   "toolu_gated",
				"name": "AskUserQuestion",
			},
		}},
	})
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()
	resolved := captureOpenOptions(t, b, transport)

	// The tool_use block must have streamed (populating lastToolUseID) before
	// the CLI raises the permission callback.
	waitForToolPart(t, b.Events(), "AskUserQuestion", 2*time.Second)

	done := make(chan any, 1)
	go func() {
		res, _ := resolved.CanUseTool(context.Background(), "AskUserQuestion", askUserQuestionInput(), nil)
		done <- res
	}()

	part := waitForQuestionPart(t, b.Events(), 2*time.Second)
	if part.ID != "toolu_gated" || part.Status != agent.PartRunning {
		t.Fatalf("tagged part = id %q status %q, want the running toolu_gated part", part.ID, part.Status)
	}
	if len(part.Question.Questions) != 1 || part.Question.Questions[0].Header != "Auth" {
		t.Fatalf("normalized questions = %+v", part.Question.Questions)
	}

	pEvt := waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second)
	pData := pEvt.Data.(agent.PermissionData)
	if pData.RequestID != part.Question.RequestID {
		t.Errorf("permission RequestID %q != part tag RequestID %q (clients suppress by matching them)",
			pData.RequestID, part.Question.RequestID)
	}
	// The legacy prompt description must show the question, not the doubled
	// tool name.
	if !strings.Contains(pData.Description, "Which auth method") {
		t.Errorf("permission Description = %q, want the first question text", pData.Description)
	}

	answers := []agent.QuestionAnswer{{Selected: []string{"JWT"}}}
	if err := b.RespondQuestion(context.Background(), part.Question.RequestID, answers, false); err != nil {
		t.Fatalf("RespondQuestion: %v", err)
	}

	select {
	case res := <-done:
		deny, ok := res.(claudecode.PermissionResultDeny)
		if !ok {
			t.Fatalf("callback result = %T, want PermissionResultDeny (deny is the answer transport)", res)
		}
		want := "Answers to your questions:\n**Auth**: JWT"
		if deny.Message != want {
			t.Errorf("deny message = %q, want %q", deny.Message, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not return after RespondQuestion")
	}
}

// Dismissing a gated question must unpark the prompt with the dismissal
// message so the model proceeds on its own judgment.
func TestClaudeCodeBackend_Question_RejectDismisses(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()
	resolved := captureOpenOptions(t, b, transport)

	done := make(chan any, 1)
	go func() {
		res, _ := resolved.CanUseTool(context.Background(), "AskUserQuestion", askUserQuestionInput(), nil)
		done <- res
	}()
	pData := waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second).Data.(agent.PermissionData)

	if err := b.RespondQuestion(context.Background(), pData.RequestID, nil, true); err != nil {
		t.Fatalf("RespondQuestion(reject): %v", err)
	}
	select {
	case res := <-done:
		deny, ok := res.(claudecode.PermissionResultDeny)
		if !ok {
			t.Fatalf("callback result = %T, want PermissionResultDeny", res)
		}
		if !strings.Contains(deny.Message, "dismissed") {
			t.Errorf("deny message = %q, want the dismissal message", deny.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not return after reject")
	}
}

func TestClaudeCodeBackend_RespondQuestion_Validation(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()
	resolved := captureOpenOptions(t, b, transport)

	if err := b.RespondQuestion(context.Background(), "does-not-exist", nil, true); err == nil {
		t.Error("RespondQuestion(unknown id) = nil, want error")
	}
	// An unknown bypass-style id with no transcript behind it must also fail
	// (the recovery path found nothing).
	if err := b.RespondQuestion(context.Background(), "q-ghost", nil, true); err == nil {
		t.Error("RespondQuestion(q-unknown) = nil, want error")
	}

	go func() {
		_, _ = resolved.CanUseTool(context.Background(), "AskUserQuestion", askUserQuestionInput(), nil)
	}()
	pData := waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second).Data.(agent.PermissionData)

	// One question, two answers → fail fast, keep the prompt pending.
	err := b.RespondQuestion(context.Background(), pData.RequestID, make([]agent.QuestionAnswer, 2), false)
	if err == nil {
		t.Error("RespondQuestion(count mismatch) = nil, want error")
	}
	// The prompt must still be answerable after the failed attempt.
	if err := b.RespondQuestion(context.Background(), pData.RequestID, nil, true); err != nil {
		t.Errorf("RespondQuestion after failed attempt: %v", err)
	}
}

// In bypassPermissions the CLI never consults CanUseTool — the question must
// surface as the completed tool part's tag (from the stream), and the answers
// must go back as a follow-up user message.
func TestClaudeCodeBackend_Question_BypassFlow(t *testing.T) {
	t.Parallel()
	const sessionID = "claude-q-bypass"
	transport := newMockTransport([]claudecode.Message{
		&claudecode.SystemMessage{
			MessageType: "system",
			Subtype:     "init",
			Data:        map[string]any{"session_id": sessionID},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":    "message_start",
				"message": map[string]any{"id": "msg_1"},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_start",
				"index": float64(0),
				"content_block": map[string]any{
					"type": "tool_use",
					"id":   "toolu_q1",
					"name": "AskUserQuestion",
				},
			},
		},
		&claudecode.StreamEvent{
			SessionID: sessionID,
			Event: map[string]any{
				"type":  "content_block_delta",
				"index": float64(0),
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": `{"questions":[{"question":"Which auth method should we use?","header":"Auth","options":[{"label":"JWT"},{"label":"Sessions"}]}]}`,
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
	})

	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()

	promptCh := make(chan string, 1)
	transport.onSend = func(prompt string) []claudecode.Message {
		select {
		case promptCh <- prompt:
		default:
		}
		return nil
	}

	_ = captureOpenOptions(t, b, transport)

	part := waitForQuestionPart(t, b.Events(), 2*time.Second)
	if part.ID != "toolu_q1" || part.Status != agent.PartCompleted {
		t.Fatalf("tagged part = id %q status %q, want completed toolu_q1", part.ID, part.Status)
	}
	if part.Question.RequestID != "q-toolu_q1" {
		t.Errorf("RequestID = %q, want q-toolu_q1 (deterministic bypass id)", part.Question.RequestID)
	}

	answers := []agent.QuestionAnswer{{Selected: []string{"Sessions"}, Custom: "prefer cookies"}}
	if err := b.RespondQuestion(context.Background(), part.Question.RequestID, answers, false); err != nil {
		t.Fatalf("RespondQuestion: %v", err)
	}

	select {
	case prompt := <-promptCh:
		want := "Answers to your questions:\n**Auth**: Sessions  (Other: prefer cookies)"
		if prompt != want {
			t.Errorf("follow-up message = %q, want %q", prompt, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no follow-up message dispatched")
	}
}
