package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

// handleKey handles keyboard input when the sidebar has focus and is
// not in branch-creation mode. Key bindings are kept in one place so
// adding new tree behavior (expand/collapse, session selection) doesn't
// scatter across renderers.
func (m *SidebarModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	msg = normalizeKeyCase(msg)

	maxIdx := len(m.flat) - 1
	if maxIdx < 0 {
		return nil
	}

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
		if m.cursor > 0 {
			m.cursor--
		} else {
			m.cursor = maxIdx
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
		if m.cursor < maxIdx {
			m.cursor++
		} else {
			m.cursor = 0
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+up"))):
		m.shiftJump(false)
	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+down"))):
		m.shiftJump(true)
	case key.Matches(msg, key.NewBinding(key.WithKeys("home", "g"))):
		m.cursor = 0
	case key.Matches(msg, key.NewBinding(key.WithKeys("end", "G"))):
		m.cursor = maxIdx
	case key.Matches(msg, key.NewBinding(key.WithKeys("space", " "))):
		// Space toggles expand without moving the cursor — useful
		// when the user wants to peek into an Older bucket without
		// losing their place. (Tab is reserved for pane switching at
		// the inbox level.)
		if m.toggleExpand() {
			return m.notifyExpandToggled()
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+enter"))):
		return m.handleShiftEnter()
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		return m.handleEnter()
	case key.Matches(msg, key.NewBinding(key.WithKeys("n"))):
		// "n" emits a compose request prefilled with the cursor's
		// worktree (or empty when the cursor isn't on a worktree-
		// shaped row — the inbox falls back to the cwd). New-worktree
		// creation lives inside the compose view's worktree picker
		// now, not as a separate inline gesture here.
		worktreePath := m.CursorWorktreePath()
		return func() tea.Msg {
			return composeRequestedMsg{worktreePath: worktreePath}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys(":"))):
		// Worktree options menu (Push/Pull) only valid on a worktree row.
		if w, ok := m.cursorNode().(worktreeNode); ok {
			localPath := w.LocalPath
			return func() tea.Msg {
				return worktreeOptionsRequestedMsg{localPath: localPath}
			}
		}
	}
	return nil
}

// handleEnter implements the per-node-kind Enter behavior:
//   - AllSessions → no-op here (inbox listens for cursor change instead)
//   - Worktree    → toggle expand
//   - Session     → emit sessionSelectedFromSidebarMsg
//   - Older bucket → toggle expand
//   - Footer rows → no-op (the inbox handles Enter on those via the
//     CursorOn* checks)
func (m *SidebarModel) handleEnter() tea.Cmd {
	n := m.cursorNode()
	if n == nil {
		return nil
	}
	switch typed := n.(type) {
	case worktreeNode, olderWorktreesNode, olderSessionsNode:
		_ = typed
		if m.toggleExpand() {
			return m.notifyExpandToggled()
		}
	case sessionNode:
		id := typed.Session.ID
		return func() tea.Msg {
			return sessionSelectedFromSidebarMsg{sessionID: id}
		}
	}
	return nil
}

// notifyExpandToggled returns a tea.Cmd that emits the persistence msg.
// Used after every expand/collapse so the inbox can write the new map
// to preferences on a background goroutine.
func (m *SidebarModel) notifyExpandToggled() tea.Cmd {
	return func() tea.Msg { return sidebarExpandToggledMsg{} }
}

// handleShiftEnter cycles through the sessions of the worktree under
// the cursor. The first press lands on the most-recent unread session
// (or the most-recent session when none are unread); subsequent presses
// step forward through the worktree's session list, wrapping at the end.
// State persists per-worktree so leaving and returning continues the
// rotation where the user left off.
func (m *SidebarModel) handleShiftEnter() tea.Cmd {
	w, ok := m.cursorNode().(worktreeNode)
	if !ok || len(w.Sessions) == 0 {
		return nil
	}
	var idx int
	if last, hasState := m.cycleIdx[w.LocalPath]; hasState {
		idx = (last + 1) % len(w.Sessions)
	} else {
		idx = firstUnreadIndex(w.Sessions)
	}
	m.cycleIdx[w.LocalPath] = idx
	id := w.Sessions[idx].ID
	return func() tea.Msg { return sessionSelectedFromSidebarMsg{sessionID: id} }
}

// firstUnreadIndex returns the first index in sessions where Unread()
// is true. Falls back to 0 when every session has been read so
// Shift+Enter always lands on a real session.
func firstUnreadIndex(sessions []agent.SessionInfo) int {
	for i, s := range sessions {
		if s.Unread() {
			return i
		}
	}
	return 0
}

// cursorNode returns the node under the cursor, or nil if the flat
// list is empty.
func (m *SidebarModel) cursorNode() sidebarNode {
	if m.cursor < 0 || m.cursor >= len(m.flat) {
		return nil
	}
	return m.flat[m.cursor]
}


