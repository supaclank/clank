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

// A gated AskUserQuestion must surface as a normalized EventQuestion before
// its EventPermission (same request id), and RespondQuestion must deliver the
// formatted answers to the model as the permission deny reason — the only
// transport that reaches a parked prompt (allow would re-run the tool's
// interactive picker headless and deadlock).
func TestClaudeCodeBackend_Question_GatedFlow(t *testing.T) {
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

	qEvt := waitForEventType(t, b.Events(), agent.EventQuestion, 2*time.Second)
	qData, ok := qEvt.Data.(agent.QuestionData)
	if !ok {
		t.Fatalf("EventQuestion data type = %T, want QuestionData", qEvt.Data)
	}
	if len(qData.Questions) != 1 || qData.Questions[0].Header != "Auth" {
		t.Fatalf("normalized questions = %+v", qData.Questions)
	}

	pEvt := waitForEventType(t, b.Events(), agent.EventPermission, 2*time.Second)
	pData := pEvt.Data.(agent.PermissionData)
	if pData.RequestID != qData.RequestID {
		t.Errorf("permission RequestID %q != question RequestID %q (clients suppress by matching them)",
			pData.RequestID, qData.RequestID)
	}
	// The legacy prompt description must show the question, not the doubled
	// tool name.
	if !strings.Contains(pData.Description, "Which auth method") {
		t.Errorf("permission Description = %q, want the first question text", pData.Description)
	}

	answers := []agent.QuestionAnswer{{Selected: []string{"JWT"}}}
	if err := b.RespondQuestion(context.Background(), qData.RequestID, answers, false); err != nil {
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

	rEvt := waitForEventType(t, b.Events(), agent.EventQuestionResolved, 2*time.Second)
	if rEvt.Data.(agent.QuestionResolvedData).RequestID != qData.RequestID {
		t.Errorf("resolved RequestID = %q, want %q",
			rEvt.Data.(agent.QuestionResolvedData).RequestID, qData.RequestID)
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
	qData := waitForEventType(t, b.Events(), agent.EventQuestion, 2*time.Second).Data.(agent.QuestionData)

	if err := b.RespondQuestion(context.Background(), qData.RequestID, nil, true); err != nil {
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

// Answering a question the old way — a plain permission deny from a legacy
// client — must still emit EventQuestionResolved so question-aware clients
// clear their card. Regression guard for mixed-client sessions.
func TestClaudeCodeBackend_Question_LegacyPermissionReplyResolves(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()
	resolved := captureOpenOptions(t, b, transport)

	go func() {
		_, _ = resolved.CanUseTool(context.Background(), "AskUserQuestion", askUserQuestionInput(), nil)
	}()
	qData := waitForEventType(t, b.Events(), agent.EventQuestion, 2*time.Second).Data.(agent.QuestionData)

	if err := b.RespondPermission(context.Background(), qData.RequestID, false, "Answers: JWT"); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	rEvt := waitForEventType(t, b.Events(), agent.EventQuestionResolved, 2*time.Second)
	if rEvt.Data.(agent.QuestionResolvedData).RequestID != qData.RequestID {
		t.Errorf("resolved RequestID = %q, want %q",
			rEvt.Data.(agent.QuestionResolvedData).RequestID, qData.RequestID)
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

	go func() {
		_, _ = resolved.CanUseTool(context.Background(), "AskUserQuestion", askUserQuestionInput(), nil)
	}()
	qData := waitForEventType(t, b.Events(), agent.EventQuestion, 2*time.Second).Data.(agent.QuestionData)

	// One question, two answers → fail fast, keep the prompt pending.
	err := b.RespondQuestion(context.Background(), qData.RequestID, make([]agent.QuestionAnswer, 2), false)
	if err == nil {
		t.Error("RespondQuestion(count mismatch) = nil, want error")
	}
	// The prompt must still be answerable after the failed attempt.
	if err := b.RespondQuestion(context.Background(), qData.RequestID, nil, true); err != nil {
		t.Errorf("RespondQuestion after failed attempt: %v", err)
	}
}

// In bypassPermissions the CLI never consults CanUseTool — the question must
// be surfaced from the stream (content_block_stop) instead, and the answers
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

	var sentPrompts []string
	promptCh := make(chan string, 1)
	transport.onSend = func(prompt string) []claudecode.Message {
		sentPrompts = append(sentPrompts, prompt)
		select {
		case promptCh <- prompt:
		default:
		}
		return nil
	}

	resolved := captureOpenOptions(t, b, transport)
	_ = resolved

	qEvt := waitForEventType(t, b.Events(), agent.EventQuestion, 2*time.Second)
	qData := qEvt.Data.(agent.QuestionData)
	if qData.RequestID != "q-toolu_q1" {
		t.Errorf("RequestID = %q, want q-toolu_q1 (stream-derived id)", qData.RequestID)
	}
	if qData.ToolUseID != "toolu_q1" {
		t.Errorf("ToolUseID = %q, want toolu_q1", qData.ToolUseID)
	}

	answers := []agent.QuestionAnswer{{Selected: []string{"Sessions"}, Custom: "prefer cookies"}}
	if err := b.RespondQuestion(context.Background(), qData.RequestID, answers, false); err != nil {
		t.Fatalf("RespondQuestion: %v", err)
	}

	select {
	case prompt := <-promptCh:
		want := "Answers to your questions:\n**Auth**: Sessions  (Other: prefer cookies)"
		if prompt != want {
			t.Errorf("follow-up message = %q, want %q", prompt, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no follow-up message dispatched; sent so far: %v", sentPrompts)
	}

	rEvt := waitForEventType(t, b.Events(), agent.EventQuestionResolved, 2*time.Second)
	if rEvt.Data.(agent.QuestionResolvedData).RequestID != qData.RequestID {
		t.Errorf("resolved RequestID = %q, want %q",
			rEvt.Data.(agent.QuestionResolvedData).RequestID, qData.RequestID)
	}
}
