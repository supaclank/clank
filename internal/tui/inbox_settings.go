package tui

// inbox_settings.go — integration code that wires the settings page and the
// color-scheme picker overlay into the inbox. Kept separate from inbox.go
// so we don't bloat that file further as more settings are added.

import (
	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// showSettings renders the Settings page in the right pane without
// shifting focus from the sidebar. Used for hover-preview when the
// cursor lands on the ⚙ row.
func (m *InboxModel) showSettings() {
	prefs, _ := config.LoadPreferences()
	remoteURL := ""
	if prefs.RemoteHub != nil {
		remoteURL = prefs.RemoteHub.URL
	}
	m.settings = newSettingsView(prefs.ColorScheme, prefs.DefaultBackend, prefs.ActiveHub, remoteURL, daemonclient.OverrideURL())
	m.settings.SetSize(m.sessionPaneWidth(), m.height)
	m.screen = screenSettings
}

// openSettings switches the inbox into the Settings screen and moves
// focus to the settings page so the user can start navigating entries
// immediately; pressing left returns focus to the sidebar.
func (m *InboxModel) openSettings() {
	m.showSettings()
	m.settings.SetFocused(true)
	m.setPane(paneSessions)
}

// closeSettings returns from the Settings screen back to the inbox list.
// Focus goes back to the sidebar so the user lands where they came from.
func (m *InboxModel) closeSettings() {
	m.screen = screenInbox
	m.setPane(paneSidebar)
	m.settings.SetFocused(false)
}

// updateSettings handles messages while the settings page is active.
// Overlay messages (theme picker) are routed here too when the overlay
// is showing, so the picker can intercept keys before the page sees them.
func (m *InboxModel) updateSettings(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Resize must always update layout, even with the theme picker open —
	// otherwise the picker swallows the WindowSizeMsg and the panes stay at
	// the pre-resize size until the picker closes.
	if wMsg, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = wMsg.Width
		m.height = wMsg.Height
		m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
		m.settings.SetSize(m.sessionPaneWidth(), m.height)
		return m, nil
	}

	// Theme picker overlay takes precedence: eat all keys while it's open.
	if m.showThemePicker {
		return m.updateThemePicker(msg)
	}

	// Provider auth modal takes precedence too.
	if m.showProviderAuth {
		return m.updateProviderAuth(msg)
	}

	switch msg := msg.(type) {
	case branchWorktreeCreatedMsg:
		cmd := m.sidebar.Update(msg)
		return m, cmd

	case settingsActivatedMsg:
		switch msg.kind {
		case settingsEntryColorScheme:
			prefs, _ := config.LoadPreferences()
			m.themePicker = newThemePicker(prefs.ColorScheme)
			m.showThemePicker = true
			return m, m.themePicker.Init()
		case settingsEntryDefaultBackend:
			// Cycle to the next backend in agent.AllBackends. Only two
			// backends today, but the cycle generalises if a third is
			// added. Derive "next" from the in-memory value, not disk:
			// rapid toggles would otherwise read stale prefs and repeat.
			// Persist synchronously so a new compose session opened
			// immediately after the toggle (which loads the backend from
			// preferences.json) sees the updated value.
			next := nextDefaultBackend(m.settings.DefaultBackendValue())
			m.settings.SetDefaultBackendValue(string(next))
			persistDefaultBackend(next)
			return m, nil
		case settingsEntryActiveHub:
			// --hub-url override wins for the whole process; toggling prefs would mislead.
			if daemonclient.OverrideURL() != "" {
				return m, nil
			}
			// Toggle local <-> remote. Persist only; user restarts the
			// TUI for it to take effect (hot-swap would orphan the SSE).
			prefs, _ := config.LoadPreferences()
			remoteURL := ""
			if prefs.RemoteHub != nil {
				remoteURL = prefs.RemoteHub.URL
			}
			next := nextActiveHub(prefs.ActiveHub)
			m.settings.SetActiveHubValue(next, remoteURL)
			persistActiveHub(next)
			return m, nil
		case settingsEntryProviders:
			m.providerAuth = newProviderAuthModel(m.client, m.hostname)
			m.showProviderAuth = true
			return m, m.providerAuth.Init()
		}
		return m, nil

	case settingsFocusSidebarMsg:
		// Move focus to the sidebar without leaving the settings screen,
		// so the user can scroll branches and come back to settings.
		m.setPane(paneSidebar)
		m.settings.SetFocused(false)
		return m, nil

	case settingsCloseMsg:
		m.closeSettings()
		return m, nil
	}

	// Sidebar-focused keys: route to sidebar handler. Cursor leaving
	// the ⚙ row while settings is showing drops back to the inbox.
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok && m.pane == paneSidebar {
		tm, cmd := m.handleSidebarKey(keyMsg)
		if m.screen == screenSettings && !m.sidebar.CursorOnSettings() {
			m.screen = screenInbox
			m.settings.SetFocused(false)
		}
		return tm, cmd
	}

	// Otherwise delegate to the settings view itself.
	var cmd tea.Cmd
	m.settings, cmd = m.settings.Update(msg)
	return m, cmd
}

// updateProviderAuth forwards messages to the provider auth modal and
// handles its terminal messages (done/cancel).
func (m *InboxModel) updateProviderAuth(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case providerAuthDoneMsg:
		m.showProviderAuth = false
		return m, nil
	case providerAuthCancelMsg:
		m.showProviderAuth = false
		return m, nil
	}
	var cmd tea.Cmd
	m.providerAuth, cmd = m.providerAuth.Update(msg)
	return m, cmd
}

// updateThemePicker forwards messages to the picker overlay and handles
// its result / cancel messages.
func (m *InboxModel) updateThemePicker(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case themePickerResultMsg:
		m.showThemePicker = false
		// The scheme is already applied (live preview); persist the name.
		go persistColorScheme(msg.scheme)
		m.settings.SetColorSchemeValue(msg.scheme)
		return m, nil

	case themePickerCancelMsg:
		// Picker has already reverted the palette; just hide.
		m.showThemePicker = false
		return m, nil
	}

	var cmd tea.Cmd
	m.themePicker, cmd = m.themePicker.Update(msg)
	return m, cmd
}

// persistColorScheme writes the user's chosen scheme to preferences.json.
// Runs in a goroutine — scheme selection should feel instant even if
// disk I/O is slow. We drop errors silently: the palette is already
// applied in-memory, and a failed write only means the scheme won't
// persist across restarts (surfaced to the user indirectly on next launch).
func persistColorScheme(name string) {
	_ = config.UpdatePreferences(func(p *config.Preferences) {
		p.ColorScheme = name
	})
}

// nextDefaultBackend returns the backend after currentName in
// agent.AllBackends, wrapping at the end. Takes the current value as a
// parameter (rather than reading prefs) so it stays in sync with the
// in-memory settings view while persistence runs asynchronously. Empty
// or unknown input resolves to agent.DefaultBackend before cycling.
func nextDefaultBackend(currentName string) agent.BackendType {
	current, _ := agent.ResolveBackendPreference(currentName)
	for i, b := range agent.AllBackends {
		if b == current {
			return agent.AllBackends[(i+1)%len(agent.AllBackends)]
		}
	}
	// Current backend not in the list (shouldn't happen) — restart cycle.
	return agent.AllBackends[0]
}

// persistDefaultBackend writes the user's chosen default backend to
// preferences.json. See persistColorScheme for the error-handling rationale.
func persistDefaultBackend(bt agent.BackendType) {
	_ = config.UpdatePreferences(func(p *config.Preferences) {
		p.DefaultBackend = string(bt)
	})
}

// nextActiveHub flips between "local" and "remote". Empty string is
// treated as "local" (the implicit default) so the first toggle moves
// to "remote".
func nextActiveHub(current string) string {
	if current == "remote" {
		return "local"
	}
	return "remote"
}

// persistActiveHub writes the user's chosen active hub to
// preferences.json. See persistColorScheme for error handling.
func persistActiveHub(name string) {
	_ = config.UpdatePreferences(func(p *config.Preferences) {
		p.ActiveHub = name
	})
}
