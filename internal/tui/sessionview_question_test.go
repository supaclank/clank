package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

func taggedQuestionPart(partID, requestID string) agent.Part {
	return agent.Part{
		ID:     partID,
		Type:   agent.PartToolCall,
		Tool:   "AskUserQuestion",
		Status: agent.PartCompleted,
		Question: &agent.QuestionPrompt{
			RequestID: requestID,
			Questions: []agent.Question{
				{
					Text:        "Which auth method should we use?",
					Header:      "Auth",
					AllowCustom: func() *bool { b := true; return &b }(),
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

func questionPartEvent(partID, requestID string) agent.Event {
	return agent.Event{
		Type:      agent.EventPartUpdate,
		Timestamp: time.Now(),
		Data:      agent.PartUpdateData{Part: taggedQuestionPart(partID, requestID)},
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

// A tagged tool part that is the transcript's last content entry is the
// active question; the conversation moving past it (a later text part)
// retires it.
func TestSessionView_ActiveQuestionDerivedFromTranscript(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)

	m.handleEvent(questionPartEvent("toolu_1", "q-toolu_1"))
	p := m.activeQuestionPart()
	if p == nil || p.Question.RequestID != "q-toolu_1" {
		t.Fatalf("activeQuestionPart = %+v, want the tagged part", p)
	}

	// The model moved on: a later assistant text entry retires the prompt.
	m.handleEvent(agent.Event{
		Type: agent.EventPartUpdate,
		Data: agent.PartUpdateData{Part: agent.Part{ID: "txt-1", Type: agent.PartText, Text: "Proceeding with JWT."}},
	})
	if p := m.activeQuestionPart(); p != nil {
		t.Fatalf("activeQuestionPart = %+v after the conversation moved on, want nil", p)
	}
}

// Reopening a session restores the card from the plain history refetch — the
// exact bug this design fixes (prompts used to live only in ephemeral
// events).
func TestSessionView_QuestionRestoredFromHistoryRefetch(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)

	m.handleSessionMessages([]agent.MessageData{
		{ID: "m1", Role: "user", Content: "help me pick auth"},
		{ID: "m2", Role: "assistant", Parts: []agent.Part{
			{ID: "m2-0", Type: agent.PartText, Text: "Sure — quick question."},
			taggedQuestionPart("toolu_hist", "q-toolu_hist"),
		}},
	})

	p := m.activeQuestionPart()
	if p == nil || p.Question.RequestID != "q-toolu_hist" {
		t.Fatalf("activeQuestionPart after refetch = %+v, want the restored question", p)
	}
}

// The permission event that gates a question (same request id, Claude
// default/plan mode) must be suppressed — the question card supersedes it.
func TestSessionView_QuestionSuppressesMatchingPermission(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)

	m.handleEvent(questionPartEvent("toolu_1", "perm-1"))
	m.handleEvent(agent.Event{
		Type: agent.EventPermission,
		Data: agent.PermissionData{
			RequestID: "perm-1", Tool: "AskUserQuestion",
			Description: "Which auth method should we use?", ToolUseID: "toolu_1",
		},
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

// Walking the prompt: digit-select on a single-select question advances to the
// next question; enter on the last question submits with the collected
// structured answers.
func TestSessionView_QuestionSelectAndSubmit(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)
	m.handleEvent(questionPartEvent("toolu_1", "q-toolu_1"))

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
	if m.replyingQuestionID != "q-toolu_1" {
		t.Fatalf("replyingQuestionID=%q, want q-toolu_1", m.replyingQuestionID)
	}

	// Applying the success result retires the prompt and records a transcript
	// entry summarizing the answers.
	m.handleQuestionReplyResult(questionReplyResultMsg{
		requestID: "q-toolu_1",
		questions: taggedQuestionPart("toolu_1", "q-toolu_1").Question.Questions,
		answers: []agent.QuestionAnswer{
			{Selected: []string{"JWT"}},
			{Selected: []string{"Search", "Export"}},
		},
	})
	if m.activeQuestionPart() != nil || m.replyingQuestionID != "" {
		t.Errorf("prompt not retired after reply")
	}
	last := m.entries[len(m.entries)-1]
	if last.kind != entryPermResult || !strings.Contains(last.content, "Auth: JWT") {
		t.Errorf("transcript entry = %+v, want an Answered summary containing 'Auth: JWT'", last)
	}
}

// Esc dismisses the prompt (best-effort reject); even a failed reject
// dismisses locally so the UI can never lock into a dead prompt.
func TestSessionView_QuestionEscRejects(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)
	m.handleEvent(questionPartEvent("toolu_1", "q-toolu_1"))

	cmd := pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("esc must dispatch a reject reply")
	}
	if m.replyingQuestionID != "q-toolu_1" {
		t.Fatalf("replyingQuestionID=%q, want q-toolu_1", m.replyingQuestionID)
	}
	m.handleQuestionReplyResult(questionReplyResultMsg{requestID: "q-toolu_1", reject: true})
	if m.activeQuestionPart() != nil {
		t.Error("prompt still active after dismissal")
	}
	last := m.entries[len(m.entries)-1]
	if last.permGranted || !strings.Contains(last.content, "Dismissed") {
		t.Errorf("transcript entry = %+v, want a Dismissed record", last)
	}
}

// A reject that errors (stale prompt, dead backend) must still dismiss
// locally — otherwise the modal card wedges the whole view.
func TestSessionView_QuestionRejectErrorStillDismisses(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)
	m.handleEvent(questionPartEvent("toolu_1", "q-toolu_1"))

	m.replyingQuestionID = "q-toolu_1"
	m.handleQuestionReplyResult(questionReplyResultMsg{
		requestID: "q-toolu_1",
		reject:    true,
		err:       context.DeadlineExceeded,
	})
	if m.activeQuestionPart() != nil {
		t.Error("failed dismissal left the prompt active (modal lock-in)")
	}
	if m.err == nil {
		t.Error("dismissal error not surfaced")
	}
}

// A failed answer keeps the prompt so the user can retry.
func TestSessionView_QuestionAnswerErrorKeepsPrompt(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)
	m.handleEvent(questionPartEvent("toolu_1", "q-toolu_1"))

	m.replyingQuestionID = "q-toolu_1"
	m.handleQuestionReplyResult(questionReplyResultMsg{
		requestID: "q-toolu_1",
		err:       context.DeadlineExceeded,
	})
	if m.activeQuestionPart() == nil {
		t.Error("failed answer dismissed the prompt; want it kept for retry")
	}
}

// Regression: dismissing a question in a session view built through the
// COMPOSE constructor (a struct literal, unlike NewSessionViewModel) panicked
// with "assignment to entry in nil map" — answeredQuestions was never
// initialized on that path.
func TestSessionView_QuestionDismissOnComposePathDoesNotPanic(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, t.TempDir(), agent.BackendClaudeCode)
	m.width, m.height = 100, 40

	m.handleEvent(questionPartEvent("toolu_1", "q-toolu_1"))
	if cmd := pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEscape}); cmd == nil {
		t.Fatal("esc must dispatch a reject reply")
	}
	m.handleQuestionReplyResult(questionReplyResultMsg{requestID: "q-toolu_1", reject: true})
	if m.activeQuestionPart() != nil {
		t.Error("prompt still active after dismissal")
	}
}

// The question card must render the options and selection state.
func TestSessionView_QuestionCardRenders(t *testing.T) {
	t.Parallel()
	m := newQuestionTestModel(t)
	m.handleEvent(questionPartEvent("toolu_1", "q-toolu_1"))
	pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})

	p := m.syncQuestionUI()
	if p == nil {
		t.Fatal("no active question to render")
	}
	card := strings.Join(m.renderQuestionCard(p), "\n")
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
