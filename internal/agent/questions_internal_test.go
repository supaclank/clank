package agent

import (
	"encoding/json"
	"testing"
	"time"

	opencode "github.com/acksell/opencode-go-sdk/sdk"
)

func TestParseClaudeQuestions_Valid(t *testing.T) {
	t.Parallel()
	input := map[string]any{
		"questions": []any{
			map[string]any{
				"question":    "Which auth method should we use?",
				"header":      "Auth",
				"multiSelect": false,
				"options": []any{
					map[string]any{"label": "JWT", "description": "Stateless tokens"},
					map[string]any{"label": "Sessions"},
				},
			},
			map[string]any{
				"question":    "Which features do you want to enable right now in v1?",
				"multiSelect": true,
				"options": []any{
					map[string]any{"label": "Search"},
					map[string]any{"label": "Export"},
				},
			},
		},
	}
	qs := parseClaudeQuestions(input)
	if len(qs) != 2 {
		t.Fatalf("got %d questions, want 2", len(qs))
	}
	q0 := qs[0]
	if q0.Text != "Which auth method should we use?" || q0.Header != "Auth" || q0.MultiSelect {
		t.Errorf("q0 = %+v, want text/header preserved and single-select", q0)
	}
	if !q0.AllowCustom {
		t.Error("Claude questions must always allow a custom answer (the tool's implicit Other)")
	}
	if len(q0.Options) != 2 || q0.Options[0].Description != "Stateless tokens" || q0.Options[1].Label != "Sessions" {
		t.Errorf("q0.Options = %+v", q0.Options)
	}
	// Missing header defaults to the first 12 chars of the question (mirrors
	// the mobile client).
	if qs[1].Header != "Which featur" {
		t.Errorf("q1.Header = %q, want first-12-chars default", qs[1].Header)
	}
	if !qs[1].MultiSelect {
		t.Error("q1 must be multi-select")
	}
}

func TestParseClaudeQuestions_InvalidShapesReturnNil(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]any{
		"no questions key": {"plan": "x"},
		"empty questions":  {"questions": []any{}},
		"non-list":         {"questions": "what?"},
		"question without text": {"questions": []any{
			map[string]any{"options": []any{map[string]any{"label": "A"}}},
		}},
		"question without valid options": {"questions": []any{
			map[string]any{"question": "Q?", "options": []any{map[string]any{"description": "no label"}}},
		}},
	}
	for name, input := range cases {
		if got := parseClaudeQuestions(input); got != nil {
			t.Errorf("%s: parseClaudeQuestions = %+v, want nil (fall back to generic permission)", name, got)
		}
	}
}

func TestFormatQuestionAnswers_Templates(t *testing.T) {
	t.Parallel()
	questions := []Question{
		{Header: "Auth", Options: []QuestionOption{{Label: "JWT"}, {Label: "Sessions"}}},
		{Header: "Features", MultiSelect: true, Options: []QuestionOption{{Label: "Search"}, {Label: "Export"}}},
	}

	got := formatQuestionAnswers(questions, []QuestionAnswer{
		{Selected: []string{"JWT"}},
		{Selected: []string{"Search", "Export"}, Custom: "also dark mode"},
	})
	want := "Answers to your questions:\n**Auth**: JWT\n**Features**: Search, Export  (Other: also dark mode)"
	if got != want {
		t.Errorf("full answers:\ngot  %q\nwant %q", got, want)
	}

	// A custom-only answer stands alone; an unanswered question is delegated.
	got = formatQuestionAnswers(questions, []QuestionAnswer{
		{Custom: "mTLS"},
		{},
	})
	want = "Answers to your questions:\n**Auth**: Other: mTLS\n**Features**: (delegated to you)"
	if got != want {
		t.Errorf("custom + delegated:\ngot  %q\nwant %q", got, want)
	}

	// Everything empty collapses to the delegation sentence.
	if got := formatQuestionAnswers(questions, []QuestionAnswer{{}, {}}); got != questionsDelegatedMessage {
		t.Errorf("all delegated: got %q, want %q", got, questionsDelegatedMessage)
	}
}

// A question surfaced via handleCanUseTool (gated mode) must not be re-emitted
// when the tool_use block's stop arrives after the prompt resolves.
func TestMaybeEmitBypassQuestion_DedupesToolUseID(t *testing.T) {
	t.Parallel()
	b := NewClaudeCodeBackend(t.TempDir())
	defer b.Stop()

	input := map[string]any{"questions": []any{
		map[string]any{"question": "Q?", "header": "Q", "options": []any{map[string]any{"label": "A"}}},
	}}

	b.maybeEmitBypassQuestion("toolu_1", ClaudeToolAskUserQuestion, input)
	select {
	case evt := <-b.Events():
		if evt.Type != EventQuestion {
			t.Fatalf("first call emitted %s, want %s", evt.Type, EventQuestion)
		}
		if evt.Data.(QuestionData).RequestID != "q-toolu_1" {
			t.Errorf("RequestID = %q, want q-toolu_1", evt.Data.(QuestionData).RequestID)
		}
	case <-time.After(time.Second):
		t.Fatal("first call emitted nothing")
	}

	b.maybeEmitBypassQuestion("toolu_1", ClaudeToolAskUserQuestion, input)
	select {
	case evt := <-b.Events():
		t.Fatalf("second call for the same tool_use emitted %s; want dedupe", evt.Type)
	case <-time.After(50 * time.Millisecond):
	}
}

// OpenCode question.asked events must map field-for-field onto QuestionData.
func TestOpenCodeHandleQuestionAsked_MapsToQuestionData(t *testing.T) {
	t.Parallel()
	b := NewOpenCodeBackend("http://127.0.0.1:0", "sess-1", nil)
	multiple, custom := true, true
	props := &opencode.GlobalEventPayloadQuestionAskedProperties{
		ID:        "req-1",
		SessionID: "sess-1",
		Questions: []*opencode.QuestionInfo{{
			Question: "Which DB?",
			Header:   "DB",
			Multiple: &multiple,
			Custom:   &custom,
			Options: []*opencode.QuestionOption{
				{Label: "Postgres", Description: "relational"},
				{Label: "SQLite"},
			},
		}},
		Tool: &opencode.QuestionTool{MessageID: "msg-1", CallID: "call-1"},
	}

	// Route through the global-event dispatcher to cover the switch wiring.
	b.handleGlobalEvent(&opencode.GlobalEvent{Payload: &opencode.GlobalEventPayload{
		QuestionAsked: &opencode.GlobalEventPayloadQuestionAsked{Properties: props},
	}})

	select {
	case evt := <-b.Events():
		if evt.Type != EventQuestion {
			t.Fatalf("event type = %s, want %s", evt.Type, EventQuestion)
		}
		data := evt.Data.(QuestionData)
		if data.RequestID != "req-1" || data.ToolUseID != "call-1" {
			t.Errorf("ids = %q/%q, want req-1/call-1", data.RequestID, data.ToolUseID)
		}
		if len(data.Questions) != 1 {
			t.Fatalf("got %d questions, want 1", len(data.Questions))
		}
		q := data.Questions[0]
		if q.Text != "Which DB?" || q.Header != "DB" || !q.MultiSelect || !q.AllowCustom {
			t.Errorf("question = %+v", q)
		}
		if len(q.Options) != 2 || q.Options[0].Description != "relational" {
			t.Errorf("options = %+v", q.Options)
		}
	case <-time.After(time.Second):
		t.Fatal("no event emitted")
	}
}

// Events for other sessions must be ignored; replied/rejected must surface as
// EventQuestionResolved for this session.
func TestOpenCodeQuestionResolved_FiltersBySession(t *testing.T) {
	t.Parallel()
	b := NewOpenCodeBackend("http://127.0.0.1:0", "sess-1", nil)

	b.handleQuestionResolved("other-session", "req-9")
	select {
	case evt := <-b.Events():
		t.Fatalf("event %s emitted for another session's question", evt.Type)
	case <-time.After(50 * time.Millisecond):
	}

	b.handleQuestionResolved("sess-1", "req-9")
	select {
	case evt := <-b.Events():
		if evt.Type != EventQuestionResolved {
			t.Fatalf("event type = %s, want %s", evt.Type, EventQuestionResolved)
		}
		if evt.Data.(QuestionResolvedData).RequestID != "req-9" {
			t.Errorf("RequestID = %q, want req-9", evt.Data.(QuestionResolvedData).RequestID)
		}
	case <-time.After(time.Second):
		t.Fatal("no resolved event emitted")
	}
}

// The question events must round-trip through the Event JSON envelope into
// their concrete payload types (what SSE clients rely on).
func TestEventJSONRoundTrip_Question(t *testing.T) {
	t.Parallel()
	src := Event{
		Type:      EventQuestion,
		Timestamp: time.Now().UTC(),
		Data: QuestionData{
			RequestID: "perm-3",
			ToolUseID: "toolu_9",
			Questions: []Question{{
				Text:        "Pick one",
				Header:      "Pick",
				AllowCustom: true,
				Options:     []QuestionOption{{Label: "A", Description: "first"}},
			}},
		},
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, ok := back.Data.(QuestionData)
	if !ok {
		t.Fatalf("decoded Data type = %T, want QuestionData", back.Data)
	}
	if data.RequestID != "perm-3" || data.ToolUseID != "toolu_9" ||
		len(data.Questions) != 1 || data.Questions[0].Options[0].Description != "first" {
		t.Errorf("round-trip mismatch: %+v", data)
	}

	resolved := Event{Type: EventQuestionResolved, Data: QuestionResolvedData{RequestID: "perm-3"}}
	raw, err = json.Marshal(resolved)
	if err != nil {
		t.Fatalf("marshal resolved: %v", err)
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal resolved: %v", err)
	}
	if rd, ok := back.Data.(QuestionResolvedData); !ok || rd.RequestID != "perm-3" {
		t.Errorf("resolved round-trip: %#v", back.Data)
	}
}
