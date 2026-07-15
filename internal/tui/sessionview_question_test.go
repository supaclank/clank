package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

func questionEvent(requestID string) agent.Event {
	return agent.Event{
		Type:      agent.EventQuestion,
		Timestamp: time.Now(),
		Data: agent.QuestionData{
			RequestID: requestID,
			ToolUseID: "toolu_1",
			Questions: []agent.Question{
				{
					Text:        "Which auth method should we use?",
					Header:      "Auth",
					AllowCustom: true,
					Options: []agent.QuestionOption{
						{Label: "JWT", Description: "Stateless tokens"},
						{Label: "Sessions"},
					},
				},
				{
					Text:        "Which features do we ship first?",
					Header:      "Features",
					MultiSelect: true,
					Options: []agent.QuestionOption{
						{Label: "Search"},
						{Label: "Export"},
					},
				},
			},
		},
	}
}

func newQuestionTestModel(t *testing.T) *SessionViewModel {
	t.Helper()
	m := NewSessionViewModel(nil, "sess-1")
	m.width, m.height = 100, 40
	return m
}

func pressKey(t *testing.T, m *SessionViewModel, msg tea.KeyPressMsg) tea.Cmd {
	t.Helper()
	_, cmd := m.handleKey(msg)
	return cmd
}

// The permission event that trails a question (same request id) must be
// suppressed — the question card supersedes the generic y/n prompt.
func TestSessionView_QuestionSuppressesMatchingPermission(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)

	m.handleEvent(questionEvent("perm-1"))
	if len(m.pendingQuestions) != 1 {
		t.Fatalf("pendingQuestions len=%d, want 1", len(m.pendingQuestions))
	}

	m.handleEvent(agent.Event{
		Type: agent.EventPermission,
		Data: agent.PermissionData{RequestID: "perm-1", Tool: "AskUserQuestion", Description: "AskUserQuestion"},
	})
	if len(m.pendingPerms) != 0 {
		t.Errorf("pendingPerms len=%d, want 0 (suppressed by the question card)", len(m.pendingPerms))
	}

	// An unrelated permission still queues.
	m.handleEvent(agent.Event{
		Type: agent.EventPermission,
		Data: agent.PermissionData{RequestID: "perm-2", Tool: "Bash", Description: "Bash: ls"},
	})
	if len(m.pendingPerms) != 1 {
		t.Errorf("pendingPerms len=%d, want 1 (unrelated prompt must not be suppressed)", len(m.pendingPerms))
	}
}

// Replayed-out-of-order delivery: if the permission lands before the question
// (SSE reconnect), the question's arrival must retroactively drop it.
func TestSessionView_QuestionDropsAlreadyQueuedPermission(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)

	m.handleEvent(agent.Event{
		Type: agent.EventPermission,
		Data: agent.PermissionData{RequestID: "perm-1", Tool: "AskUserQuestion", Description: "AskUserQuestion"},
	})
	m.handleEvent(questionEvent("perm-1"))

	if len(m.pendingPerms) != 0 {
		t.Errorf("pendingPerms len=%d, want 0 after question arrived", len(m.pendingPerms))
	}
	if len(m.pendingQuestions) != 1 {
		t.Errorf("pendingQuestions len=%d, want 1", len(m.pendingQuestions))
	}
}

// Walking the prompt: digit-select on a single-select question advances to the
// next question; enter on the last question submits with the collected
// structured answers.
func TestSessionView_QuestionSelectAndSubmit(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)
	m.handleEvent(questionEvent("perm-1"))

	// "1" picks JWT on the single-select first question and auto-advances.
	if cmd := pressKey(t, m, tea.KeyPressMsg{Code: '1', Text: "1"}); cmd != nil {
		t.Fatal("digit select must not dispatch a reply yet")
	}
	if m.questionIdx != 1 {
		t.Fatalf("questionIdx=%d, want 1 (auto-advance after single-select pick)", m.questionIdx)
	}

	// Multi-select: toggle both options.
	pressKey(t, m, tea.KeyPressMsg{Code: '1', Text: "1"})
	pressKey(t, m, tea.KeyPressMsg{Code: '2', Text: "2"})
	if m.questionIdx != 1 {
		t.Fatalf("questionIdx=%d, want still 1 (multi-select must not auto-advance)", m.questionIdx)
	}

	// Enter on the last question submits.
	cmd := pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on the last question must dispatch the reply")
	}
	if m.replyingQuestionID != "perm-1" {
		t.Fatalf("replyingQuestionID=%q, want perm-1", m.replyingQuestionID)
	}

	// Applying the success result clears the prompt and records a transcript
	// entry summarizing the answers.
	m.handleQuestionReplyResult(questionReplyResultMsg{
		question: m.pendingQuestions[0],
		answers: []agent.QuestionAnswer{
			{Selected: []string{"JWT"}},
			{Selected: []string{"Search", "Export"}},
		},
	})
	if len(m.pendingQuestions) != 0 || m.replyingQuestionID != "" {
		t.Errorf("prompt not cleared: questions=%d replying=%q", len(m.pendingQuestions), m.replyingQuestionID)
	}
	if len(m.entries) == 0 {
		t.Fatal("no transcript entry recorded for the reply")
	}
	last := m.entries[len(m.entries)-1]
	if last.kind != entryPermResult || !strings.Contains(last.content, "Auth: JWT") {
		t.Errorf("transcript entry = %+v, want an Answered summary containing 'Auth: JWT'", last)
	}
}

// Esc dismisses the whole prompt as a reject.
func TestSessionView_QuestionEscRejects(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)
	m.handleEvent(questionEvent("perm-1"))

	cmd := pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("esc must dispatch a reject reply")
	}
	if m.replyingQuestionID != "perm-1" {
		t.Fatalf("replyingQuestionID=%q, want perm-1", m.replyingQuestionID)
	}
	m.handleQuestionReplyResult(questionReplyResultMsg{
		question: questionEvent("perm-1").Data.(agent.QuestionData),
		reject:   true,
	})
	last := m.entries[len(m.entries)-1]
	if last.permGranted || !strings.Contains(last.content, "Dismissed") {
		t.Errorf("transcript entry = %+v, want a Dismissed record", last)
	}
}

// A question.resolved event (answered on another client) must clear the card.
func TestSessionView_QuestionResolvedEventClears(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)
	m.handleEvent(questionEvent("perm-1"))

	m.handleEvent(agent.Event{
		Type: agent.EventQuestionResolved,
		Data: agent.QuestionResolvedData{RequestID: "perm-1"},
	})
	if len(m.pendingQuestions) != 0 {
		t.Errorf("pendingQuestions len=%d, want 0 after question.resolved", len(m.pendingQuestions))
	}
}

// The question card must render the options and selection state.
func TestSessionView_QuestionCardRenders(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)
	m.handleEvent(questionEvent("perm-1"))
	pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})

	card := strings.Join(m.renderQuestionCard(), "\n")
	for _, want := range []string{"Auth", "Which auth method", "JWT", "Sessions", "Other"} {
		if !strings.Contains(card, want) {
			t.Errorf("question card missing %q:\n%s", want, card)
		}
	}
}

// An ExitPlanMode permission renders the plan (from the gated tool part) with
// approve / request-changes / deny choices, and 'r' collects notes that ride
// the deny message.
func TestSessionView_PlanReviewPrompt(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)

	// The gated tool part carries the plan input (emitted by the backend while
	// the prompt parks).
	m.handleEvent(agent.Event{
		Type: agent.EventPartUpdate,
		Data: agent.PartUpdateData{Part: agent.Part{
			ID:     "toolu_plan",
			Type:   agent.PartToolCall,
			Tool:   agent.ClaudeToolExitPlanMode,
			Status: agent.PartRunning,
			Input:  map[string]any{"plan": "1. Ship the thing\n2. Test it"},
		}},
	})
	m.handleEvent(agent.Event{
		Type: agent.EventPermission,
		Data: agent.PermissionData{
			RequestID: "perm-9", Tool: agent.ClaudeToolExitPlanMode,
			Description: "ExitPlanMode", ToolUseID: "toolu_plan",
		},
	})

	if got := m.planTextFor(m.pendingPerms[0]); !strings.Contains(got, "Ship the thing") {
		t.Fatalf("planTextFor = %q, want the plan input", got)
	}

	// 'r' opens the notes input instead of replying.
	if cmd := pressKey(t, m, tea.KeyPressMsg{Code: 'r', Text: "r"}); cmd != nil {
		t.Fatal("'r' must not dispatch a reply")
	}
	if !m.planNotesActive {
		t.Fatal("planNotesActive not set after 'r'")
	}

	// Type a note and submit with enter → deny with the revise template.
	m.questionInput.SetValue("Use sessions instead")
	cmd := pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter must dispatch the deny-with-notes reply")
	}
	if m.replyingPermID != "perm-9" {
		t.Errorf("replyingPermID=%q, want perm-9", m.replyingPermID)
	}
	if m.planNotesActive {
		t.Error("planNotesActive still set after submit")
	}
}

func TestFormatPlanRevisionNotes(t *testing.T) {
	t.Parallel()
	got := formatPlanRevisionNotes("  tighten scope  ")
	want := "Here's my review of the plan. Please revise it based on the comments below.\n\ntighten scope"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := formatPlanRevisionNotes("   "); got != "Please revise the plan." {
		t.Errorf("empty notes: got %q", got)
	}
}
