package tui

// Regression tests for pane-shortcut keys leaking past an active blocking
// prompt: with a permission prompt on screen the compose textarea is
// deliberately released (inputActive == false) so y/n reach the prompt —
// the inbox-level "n"/"w"/"left"/Tab intercepts must not steal them.

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

// newInboxWithPermissionPrompt builds an InboxModel showing a session whose
// permission prompt awaits an answer (textarea released, y/n keys live).
func newInboxWithPermissionPrompt() *InboxModel {
	m := &InboxModel{
		width:        120,
		height:       40,
		screen:       screenSession,
		activeConnID: "s1",
	}
	m.setPane(paneSessions)
	m.sessionView = NewSessionViewModel(nil, "s1")
	m.sessionView.pendingPerms = []agent.PermissionData{
		{RequestID: "p1", Tool: "bash", Description: "rm -rf"},
	}
	return m
}

// Pressing "n" while a permission prompt is up must deny the permission,
// not open the compose view.
func TestSessionKeys_PermissionPromptN_DeniesInsteadOfCompose(t *testing.T) {
	t.Parallel()

	m := newInboxWithPermissionPrompt()
	model, _ := m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	m = model.(*InboxModel)

	if m.activeCompose {
		t.Fatal("'n' opened the compose view instead of denying the permission")
	}
	if m.sessionView.replyingPermID != "p1" {
		t.Errorf("replyingPermID = %q, want %q (deny not dispatched)", m.sessionView.replyingPermID, "p1")
	}
}

// Shift+N (compose on a fresh worktree) must also stay locked out while a
// permission prompt is up.
func TestSessionKeys_PermissionPromptShiftN_DoesNotCompose(t *testing.T) {
	t.Parallel()

	m := newInboxWithPermissionPrompt()
	model, _ := m.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModShift, Text: "N"})
	m = model.(*InboxModel)

	if m.activeCompose {
		t.Fatal("Shift+N opened the compose view over an active permission prompt")
	}
	if m.sessionView.replyingPermID != "" {
		t.Errorf("replyingPermID = %q, want empty (Shift+N is not a deny)", m.sessionView.replyingPermID)
	}
}

// While typing ExitPlanMode revision notes (a text-entry sub-mode where
// inputActive is false), letters bound to pane shortcuts must land in the
// notes input — "n" must neither deny nor open compose, "w" must not
// toggle the sidebar.
func TestSessionKeys_PlanNotesTyping_KeysReachNotesInput(t *testing.T) {
	t.Parallel()

	m := newInboxWithPermissionPrompt()
	m.sessionView.pendingPerms[0].Tool = agent.ClaudeToolExitPlanMode
	m.sessionView.planNotesActive = true
	m.sessionView.questionInput = newQuestionTextInput("notes")
	m.sessionView.questionInput.Focus()

	for _, key := range []tea.KeyPressMsg{
		{Code: 'n', Text: "n"},
		{Code: 'w', Text: "w"},
	} {
		model, _ := m.Update(key)
		m = model.(*InboxModel)
	}

	if m.activeCompose {
		t.Fatal("typing plan notes opened the compose view")
	}
	if m.sessionView.replyingPermID != "" {
		t.Errorf("replyingPermID = %q, want empty (typed 'n' is not a deny)", m.sessionView.replyingPermID)
	}
	if got := m.sessionView.questionInput.Value(); got != "nw" {
		t.Errorf("notes input = %q, want %q", got, "nw")
	}
}

// Tab must not swap focus to the sidebar while a prompt owns the keyboard.
func TestSessionKeys_PermissionPromptTab_DoesNotSwapPane(t *testing.T) {
	t.Parallel()

	m := newInboxWithPermissionPrompt()
	if !m.showTwoPanes() {
		t.Fatal("test setup: expected two-pane layout")
	}
	model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = model.(*InboxModel)

	if m.pane != paneSessions {
		t.Errorf("pane = %v, want paneSessions (Tab must stay locked out during a prompt)", m.pane)
	}
}
