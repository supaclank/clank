package tui

// Regression tests for pane-shortcut keys leaking past an active blocking
// prompt: with a permission prompt on screen the compose textarea is
// deliberately released (inputActive == false) so y/n reach the prompt —
// the inbox-level "n"/"w"/"left"/Tab intercepts must not steal them.

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
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
