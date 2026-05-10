package tui

// inbox_cloud.go — integration code wiring the cloud panel into the
// inbox. Mirrors inbox_settings.go: hover-preview via showCloud,
// focused-page via openCloud, exit via closeCloud, and an updateCloud
// dispatcher that delegates to the cloudView model while routing
// sidebar keys to the sidebar.

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// showCloud renders the Cloud panel in the right pane without shifting
// focus from the sidebar. Used for hover-preview when the cursor lands
// on the ☁ row.
//
// The cloud view is now eagerly initialized in InboxModel.Init so a
// saved session gets verified in the background. We only set the size
// and screen here; the cloudInitialized fallback remains for safety in
// case Init was bypassed.
func (m *InboxModel) showCloud() tea.Cmd {
	if !m.cloudInitialized {
		m.cloud = newCloudView()
		m.cloud.SetSize(m.sessionPaneWidth(), m.height)
		m.screen = screenCloud
		m.cloudInitialized = true
		return m.cloud.Init()
	}
	m.cloud.SetSize(m.sessionPaneWidth(), m.height)
	m.screen = screenCloud
	return nil
}

// openCloud switches the inbox into the Cloud screen and moves focus
// to the panel so the user can start typing immediately. Returns the
// init Cmd so the panel can kick off async work (loading prefs,
// attempting an /me with a saved session).
func (m *InboxModel) openCloud() tea.Cmd {
	cmd := m.showCloud()
	m.cloud.SetFocused(true)
	m.setPane(paneSessions)
	return cmd
}

// closeCloud returns from the Cloud screen back to the inbox list.
// Focus goes back to the sidebar so the user lands where they came from.
func (m *InboxModel) closeCloud() {
	m.screen = screenInbox
	m.setPane(paneSidebar)
	m.cloud.SetFocused(false)
}

// updateCloud handles messages while the cloud panel is active.
// Sidebar-focused keys are forwarded to the sidebar so the user can
// navigate up/down between footer rows without first leaving the panel.
func (m *InboxModel) updateCloud(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Resize must always update layout.
	if wMsg, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = wMsg.Width
		m.height = wMsg.Height
		m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
		m.cloud.SetSize(m.sessionPaneWidth(), m.height)
		return m, nil
	}

	// Global key handling that the cloud view itself doesn't own.
	// Quit and "back to inbox" must work from any cloud phase so the
	// user is never stuck. Phase-specific keys (Enter to start the
	// flow, c/esc to cancel awaiting, etc.) still flow through to
	// cloudView below.
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("ctrl+c", "q"))):
			return m, tea.Quit
		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("left"))):
			// Left-arrow returns focus to the sidebar without
			// closing the panel — mirrors the settings UX so the
			// user can scroll branches and come back.
			m.setPane(paneSidebar)
			m.cloud.SetFocused(false)
			return m, nil
		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("esc"))):
			// Esc closes the cloud panel back to the inbox unless
			// the cloud view wants to consume it (e.g. cancel an
			// in-flight device flow during cloudPhaseAwaiting). The
			// cloud view's internal esc handling fires first; if it
			// transitions phase, the next key event will land on the
			// usual paths.
			if m.cloud.phase != cloudPhaseAwaiting {
				m.closeCloud()
				return m, nil
			}
		}
	}

	// Sidebar-focused keys: route to sidebar handler. Cursor leaving
	// the ☁ row while the cloud is showing drops back to the inbox
	// (mirrors the settings transition).
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok && m.pane == paneSidebar {
		tm, cmd := m.handleSidebarKey(keyMsg)
		if m.screen == screenCloud && !m.sidebar.CursorOnCloud() {
			m.screen = screenInbox
			m.cloud.SetFocused(false)
		}
		return tm, cmd
	}

	// Otherwise delegate to the cloud view itself.
	var cmd tea.Cmd
	m.cloud, cmd = m.cloud.Update(msg)
	// cloudView.Status combines the disk-derived identity baseline
	// with in-memory reachability tracking, so the sidebar reflects
	// both auth state (online/offline) and server health
	// (checking/unavailable) without the sidebar owning either.
	m.sidebar.SetCloudStatus(m.cloud.Status())
	return m, cmd
}
