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
			t.Errorf("%s: parseClaudeQuestions = %+v, want nil (fall back to generic tool card)", name, got)
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

func TestTagQuestionPart(t *testing.T) {
	t.Parallel()
	input := map[string]any{"questions": []any{
		map[string]any{"question": "Q?", "header": "Q", "options": []any{map[string]any{"label": "A"}}},
	}}

	tag := tagQuestionPart("toolu_1", ClaudeToolAskUserQuestion, input)
	if tag == nil {
		t.Fatal("valid AskUserQuestion input produced no tag")
	}
	if tag.RequestID != "q-toolu_1" {
		t.Errorf("RequestID = %q, want q-toolu_1 (deterministic, transcript-recoverable)", tag.RequestID)
	}
	if len(tag.Questions) != 1 || tag.Questions[0].Header != "Q" {
		t.Errorf("Questions = %+v", tag.Questions)
	}

	if tagQuestionPart("toolu_1", "Bash", map[string]any{"command": "ls"}) != nil {
		t.Error("non-question tool must not be tagged")
	}
	if tagQuestionPart("toolu_1", ClaudeToolAskUserQuestion, map[string]any{"questions": "junk"}) != nil {
		t.Error("unparseable input must not be tagged")
	}
	if tagQuestionPart("", ClaudeToolAskUserQuestion, input) != nil {
		t.Error("missing tool_use id must not be tagged (no addressable request id)")
	}
}

// opencodeQuestionToolPart builds an SDK part for the question tool call.
func opencodeQuestionToolPart(callID string) *opencode.Part {
	return &opencode.Part{
		Tool: &opencode.ToolPart{
			ID:     "prt_1",
			CallID: callID,
			Tool:   OpenCodeToolQuestion,
			State:  &opencode.ToolState{Running: &opencode.ToolStateRunning{}},
		},
	}
}

func opencodeQuestionAskedProps(callID, requestID string) *opencode.GlobalEventPayloadQuestionAskedProperties {
	multiple, custom := true, true
	return &opencode.GlobalEventPayloadQuestionAskedProperties{
		ID:        requestID,
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
		Tool: &opencode.QuestionTool{MessageID: "msg-1", CallID: callID},
	}
}

// question.asked arriving BEFORE the tool part: the part must come out of
// conversion already tagged.
func TestOpenCodeQuestionTag_AskedThenPart(t *testing.T) {
	t.Parallel()
	b := NewOpenCodeBackend("http://127.0.0.1:0", "sess-1", nil)

	b.handleGlobalEvent(&opencode.GlobalEvent{Payload: &opencode.GlobalEventPayload{
		QuestionAsked: &opencode.GlobalEventPayloadQuestionAsked{Properties: opencodeQuestionAskedProps("call-1", "req-1")},
	}})

	part := b.convertSDKPart(opencodeQuestionToolPart("call-1"))
	if part == nil || part.Question == nil {
		t.Fatalf("converted part = %+v, want a question tag", part)
	}
	if part.Question.RequestID != "req-1" {
		t.Errorf("RequestID = %q, want req-1", part.Question.RequestID)
	}
	q := part.Question.Questions[0]
	if q.Text != "Which DB?" || q.Header != "DB" || !q.MultiSelect || !q.AllowCustom {
		t.Errorf("question = %+v", q)
	}
	if len(q.Options) != 2 || q.Options[0].Description != "relational" {
		t.Errorf("options = %+v", q.Options)
	}
}

// The tool part arriving BEFORE question.asked: registering the prompt must
// re-emit the cached part with the tag so live clients aren't left untagged.
func TestOpenCodeQuestionTag_PartThenAsked(t *testing.T) {
	t.Parallel()
	b := NewOpenCodeBackend("http://127.0.0.1:0", "sess-1", nil)

	if part := b.convertSDKPart(opencodeQuestionToolPart("call-1")); part.Question != nil {
		t.Fatal("part converted before question.asked must not be tagged yet")
	}

	b.handleGlobalEvent(&opencode.GlobalEvent{Payload: &opencode.GlobalEventPayload{
		QuestionAsked: &opencode.GlobalEventPayloadQuestionAsked{Properties: opencodeQuestionAskedProps("call-1", "req-1")},
	}})

	select {
	case evt := <-b.Events():
		if evt.Type != EventPartUpdate {
			t.Fatalf("event type = %s, want %s", evt.Type, EventPartUpdate)
		}
		part := evt.Data.(PartUpdateData).Part
		if part.ID != "prt_1" || part.Question == nil || part.Question.RequestID != "req-1" {
			t.Errorf("re-emitted part = %+v, want prt_1 tagged req-1", part)
		}
	case <-time.After(time.Second):
		t.Fatal("no tagged part re-emitted after question.asked")
	}
}

// Events for other sessions must not register prompts.
func TestOpenCodeQuestionTag_FiltersBySession(t *testing.T) {
	t.Parallel()
	b := NewOpenCodeBackend("http://127.0.0.1:0", "sess-1", nil)

	props := opencodeQuestionAskedProps("call-9", "req-9")
	props.SessionID = "other-session"
	b.handleGlobalEvent(&opencode.GlobalEvent{Payload: &opencode.GlobalEventPayload{
		QuestionAsked: &opencode.GlobalEventPayloadQuestionAsked{Properties: props},
	}})

	if part := b.convertSDKPart(opencodeQuestionToolPart("call-9")); part.Question != nil {
		t.Errorf("another session's question tagged our part: %+v", part.Question)
	}
}

// A tagged part must round-trip through the Event JSON envelope with the tag
// intact (what SSE clients and the Messages() endpoint rely on).
func TestPartQuestionJSONRoundTrip(t *testing.T) {
	t.Parallel()
	src := Event{
		Type:      EventPartUpdate,
		Timestamp: time.Now().UTC(),
		Data: PartUpdateData{Part: Part{
			ID:     "toolu_9",
			Type:   PartToolCall,
			Tool:   ClaudeToolAskUserQuestion,
			Status: PartRunning,
			Question: &QuestionPrompt{
				RequestID: "perm-3",
				Questions: []Question{{
					Text:        "Pick one",
					Header:      "Pick",
					AllowCustom: true,
					Options:     []QuestionOption{{Label: "A", Description: "first"}},
				}},
			},
		}},
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	part := back.Data.(PartUpdateData).Part
	if part.Question == nil || part.Question.RequestID != "perm-3" ||
		len(part.Question.Questions) != 1 || part.Question.Questions[0].Options[0].Description != "first" {
		t.Errorf("round-trip mismatch: %+v", part.Question)
	}
}
