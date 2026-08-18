package tui

// connect.go — `clank connect` as a standalone bubbletea program.
//
// The inbox reaches the provider-auth flow as a modal over a running
// TUI; the CLI has no inbox to overlay. ConnectModel is the missing
// shell: it owns the root-program concerns the inbox used to handle
// (ctrl+c, quitting on done/cancel, reporting the outcome) and adds the
// backend picker that precedes the flow when the user didn't name a
// backend. The auth flow itself is providerAuthModel, unchanged.
//
//   pickBackend → allow harness → detect auth → provider auth when needed
//
// A ConnectModel built with a backend skips the picker but still asks for
// permission before it checks authentication.

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/config"
)

// ConnectResult is what the CLI reads off the model after the program
// exits. Backend is set as soon as the user picks one, so a canceled
// run still reports what they were connecting.
type ConnectResult struct {
	Backend      agent.BackendType
	IsAllowed    bool
	IsConnected  bool
	ProviderName string
}

// connectPhase tracks the allow, detection, and authentication flow. The
// provider phase delegates every message to providerAuthModel.
type connectPhase int

const (
	// connectPhaseLoading reads permissions and fetches authentication status
	// only for harnesses the user already allowed.
	connectPhaseLoading connectPhase = iota
	connectPhasePickBackend
	connectPhaseAllow
	connectPhaseDetecting
	connectPhaseProvider
	connectPhaseReady
	connectPhaseError
)

const (
	connectPickerQuestion     = "Which harnesses should Clank be allowed to connect to?"
	connectNeedsApprovalLabel = "needs approval"
	connectPickerHelp         = "↑↓ navigate · enter review · q quit"
	connectAllowQuestion      = "Allow Clank to connect to and control %s?\n"
)

// connectProvidersLoadedMsg carries the picker's own catalog read.
// Distinct from providerListLoadedMsg so a late arrival can never be
// mistaken for the hosted model's list load.
type connectProvidersLoadedMsg struct {
	providers []agent.ProviderAuthInfo
	allowed   map[agent.BackendType]bool
	err       error
}

type connectHarnessAllowedMsg struct {
	backend   agent.BackendType
	providers []agent.ProviderAuthInfo
	err       error
}

// ConnectModel drives `clank connect` end to end.
type ConnectModel struct {
	caller ProviderAuthCaller

	phase  connectPhase
	errMsg string

	// backends is the picker's row set: every backend Clank can launch,
	// paired with its permission and selected authentication provider.
	backends []connectBackendRow
	cursor   int

	// hasPicker records that the run began at the backend picker, which
	// is what makes stepping back out of the provider flow meaningful. A
	// run started with a named backend has nothing behind it.
	hasPicker bool

	providerAuth providerAuthModel
	result       ConnectResult

	spinner spinner.Model
}

// connectBackendRow is one picker row.
type connectBackendRow struct {
	Backend      agent.BackendType
	IsAllowed    bool
	ProviderName string
}

// NewConnectModel returns the connect program. A non-empty backend skips the
// picker; an empty one starts with the full harness list.
func NewConnectModel(caller ProviderAuthCaller, backend agent.BackendType) *ConnectModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(primaryColor)

	m := &ConnectModel{caller: caller, spinner: sp, phase: connectPhaseLoading}
	if backend != "" {
		m.result.Backend = backend
		return m
	}
	m.hasPicker = true
	return m
}

// Result reports what the run achieved. Read after the program exits.
func (m *ConnectModel) Result() ConnectResult {
	if m.providerAuth.phase == providerPhaseSuccess {
		m.result.IsAllowed = true
		m.result.IsConnected = true
		m.result.ProviderName = m.providerAuth.activeProvider.DisplayName
	}
	return m.result
}

func (m *ConnectModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.loadBackendsCmd())
}

func (m *ConnectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case providerAuthDoneMsg:
		m.result.IsAllowed = true
		m.result.IsConnected = true
		m.result.ProviderName = m.providerAuth.activeProvider.DisplayName
		m.updateBackendRow(m.result.Backend, true, m.result.ProviderName)
		if m.hasPicker {
			m.phase = connectPhasePickBackend
			m.providerAuth = providerAuthModel{}
			return m, nil
		}
		return m, tea.Quit

	case providerAuthCancelMsg:
		// Leaving the provider list steps back to the backend picker
		// when there is one — a user who picked the wrong agent wants
		// the question again, not the program gone.
		if m.hasPicker && m.phase == connectPhaseProvider {
			m.phase = connectPhasePickBackend
			m.providerAuth = providerAuthModel{}
			return m, nil
		}
		return m, tea.Quit

	case connectProvidersLoadedMsg:
		if msg.err != nil {
			m.phase = connectPhaseError
			m.errMsg = msg.err.Error()
			return m, nil
		}
		m.backends = connectBackendRows(msg.providers, msg.allowed)
		if m.hasPicker {
			m.phase = connectPhasePickBackend
			return m, nil
		}
		row := m.backendRow(m.result.Backend)
		m.result.IsAllowed = row.IsAllowed
		m.result.ProviderName = row.ProviderName
		m.result.IsConnected = row.ProviderName != ""
		if row.IsAllowed {
			return m.enterProvider(m.result.Backend)
		}
		m.phase = connectPhaseAllow
		return m, nil

	case connectHarnessAllowedMsg:
		if msg.err != nil {
			m.phase = connectPhaseError
			m.errMsg = msg.err.Error()
			return m, nil
		}
		providerName := connectedProviderName(msg.providers)
		m.result.Backend = msg.backend
		m.result.IsAllowed = true
		m.result.IsConnected = providerName != ""
		m.result.ProviderName = providerName
		m.updateBackendRow(msg.backend, true, providerName)
		if providerName != "" {
			if m.hasPicker {
				m.phase = connectPhasePickBackend
				return m, nil
			}
			m.phase = connectPhaseReady
			return m, nil
		}
		return m.enterProvider(msg.backend)

	case tea.KeyPressMsg:
		// Quitting is the root program's business — the inbox owned it
		// before, and providerAuthModel only knows esc.
		if m.isQuitKey(msg) {
			return m, m.quitCmd()
		}
		if m.phase != connectPhaseProvider {
			return m.handleKey(msg)
		}
	}

	if m.phase == connectPhaseProvider {
		// TODO(ai-review): a stale async message from a previous backend's flow can leak into a freshly reselected one — https://github.com/supaclank/clank/pull/213#discussion_r3696748681
		var cmd tea.Cmd
		m.providerAuth, cmd = m.providerAuth.Update(msg)
		return m, cmd
	}

	if _, ok := msg.(spinner.TickMsg); ok {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

// isQuitKey reports whether msg ends the program. ctrl+c always does;
// "q" does everywhere except a focused text field, where it is a
// character the user is typing (an API key may well contain one).
func (m *ConnectModel) isQuitKey(msg tea.KeyPressMsg) bool {
	if key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))) {
		return true
	}
	if m.phase == connectPhaseProvider && m.providerAuth.acceptsTextInput() {
		return false
	}
	return key.Matches(msg, key.NewBinding(key.WithKeys("q", "Q")))
}

// quitCmd ends the program, first aborting any flow still running on
// the host: this process exiting does not stop a device poll or a
// setup-token PTY that clank-host started on its behalf.
func (m *ConnectModel) quitCmd() tea.Cmd {
	if m.phase == connectPhaseProvider && m.providerAuth.hasLiveFlow() {
		return tea.Sequence(m.providerAuth.cancelFlowCmd(), tea.Quit)
	}
	return tea.Quit
}

func (m *ConnectModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	msg = normalizeKeyCase(msg)
	if key.Matches(msg, key.NewBinding(key.WithKeys("esc"))) && m.phase != connectPhaseAllow {
		return m, tea.Quit
	}

	switch m.phase {
	case connectPhaseError:
		// Any key dismisses; the CLI reports the failure.
		return m, tea.Quit

	case connectPhasePickBackend:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
			if m.cursor > 0 {
				m.cursor--
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
			if m.cursor < len(m.backends)-1 {
				m.cursor++
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			if m.cursor >= 0 && m.cursor < len(m.backends) {
				row := m.backends[m.cursor]
				backend := row.Backend
				m.result.Backend = backend
				m.result.IsAllowed = row.IsAllowed
				m.result.IsConnected = row.ProviderName != ""
				m.result.ProviderName = row.ProviderName
				if row.IsAllowed {
					return m.enterProvider(backend)
				}
				m.phase = connectPhaseAllow
			}
		}

	case connectPhaseAllow:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("y", "Y", "enter"))):
			m.phase = connectPhaseDetecting
			return m, m.allowHarnessCmd(m.result.Backend)
		case key.Matches(msg, key.NewBinding(key.WithKeys("n", "N", "esc"))):
			if m.hasPicker {
				m.phase = connectPhasePickBackend
				return m, nil
			}
			return m, tea.Quit
		}

	case connectPhaseReady:
		return m, tea.Quit
	}
	return m, nil
}

func (m *ConnectModel) loadBackendsCmd() tea.Cmd {
	caller := m.caller
	return func() tea.Msg {
		prefs, err := config.LoadPreferences()
		if err != nil {
			return connectProvidersLoadedMsg{err: fmt.Errorf("load harness permissions: %w", err)}
		}
		allowed := make(map[agent.BackendType]bool, len(agent.AllBackends))
		var providers []agent.ProviderAuthInfo
		for _, bt := range agent.AllBackends {
			if !prefs.IsHarnessAllowed(string(bt)) {
				continue
			}
			allowed[bt] = true
			ctx, cancel := context.WithTimeout(context.Background(), providerListLoadTimeout)
			found, listErr := caller.ListAuthProviders(ctx, bt)
			cancel()
			if listErr != nil {
				return connectProvidersLoadedMsg{err: listErr}
			}
			providers = append(providers, found...)
		}
		return connectProvidersLoadedMsg{providers: providers, allowed: allowed}
	}
}

func (m *ConnectModel) allowHarnessCmd(backend agent.BackendType) tea.Cmd {
	caller := m.caller
	return func() tea.Msg {
		if err := config.UpdatePreferences(func(p *config.Preferences) {
			p.SetHarnessAllowed(string(backend), true)
		}); err != nil {
			return connectHarnessAllowedMsg{backend: backend, err: fmt.Errorf("allow %s: %w", backend, err)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), providerListLoadTimeout)
		defer cancel()
		providers, err := caller.ListAuthProviders(ctx, backend)
		return connectHarnessAllowedMsg{backend: backend, providers: providers, err: err}
	}
}

func (m *ConnectModel) enterProvider(backend agent.BackendType) (*ConnectModel, tea.Cmd) {
	m.phase = connectPhaseProvider
	m.providerAuth = newProviderAuthModel(m.caller, backend, "")
	return m, m.providerAuth.Init()
}

func (m *ConnectModel) backendRow(backend agent.BackendType) connectBackendRow {
	for _, row := range m.backends {
		if row.Backend == backend {
			return row
		}
	}
	return connectBackendRow{Backend: backend}
}

func (m *ConnectModel) updateBackendRow(backend agent.BackendType, allowed bool, providerName string) {
	for i := range m.backends {
		if m.backends[i].Backend == backend {
			m.backends[i].IsAllowed = allowed
			m.backends[i].ProviderName = providerName
			return
		}
	}
}

// connectBackendRows pairs every launchable backend with its permission and
// authentication state. Disallowed backends remain visible as allow actions.
func connectBackendRows(providers []agent.ProviderAuthInfo, allowed map[agent.BackendType]bool) []connectBackendRow {
	rows := make([]connectBackendRow, 0, len(agent.AllBackends))
	for _, bt := range agent.AllBackends {
		rows = append(rows, connectBackendRow{
			Backend:      bt,
			IsAllowed:    allowed[bt],
			ProviderName: connectedProviderNameForBackend(providers, bt),
		})
	}
	return rows
}

func connectedProviderName(providers []agent.ProviderAuthInfo) string {
	for _, p := range providers {
		if p.Connected {
			return p.DisplayName
		}
	}
	return ""
}

func connectedProviderNameForBackend(providers []agent.ProviderAuthInfo, backend agent.BackendType) string {
	for _, p := range providers {
		if p.Backend == backend && p.Connected {
			return p.DisplayName
		}
	}
	return ""
}

// View renders inline (no alt screen) so the connect flow stays in the
// terminal's scrollback alongside whatever ran before it — `clank
// preview` prints around this.
func (m *ConnectModel) View() tea.View {
	return connectView(m.body())
}

// connectView wraps rendered body text in the inline, mouse-free
// tea.View every phase shares.
func connectView(body string) tea.View {
	v := tea.NewView(strings.TrimRight(body, "\n"))
	v.AltScreen = false
	v.MouseMode = tea.MouseModeNone
	return v
}

func (m *ConnectModel) body() string {
	if m.phase == connectPhaseProvider {
		return m.providerAuth.View()
	}

	const menuWidth = 60
	innerWidth := menuWidth - 4

	var sb strings.Builder
	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(textColor).
		Width(innerWidth).Render("Agent harnesses"))
	sb.WriteString("\n\n")

	switch m.phase {
	case connectPhaseLoading:
		sb.WriteString(m.spinner.View() + " Loading harness permissions…")

	case connectPhaseDetecting:
		sb.WriteString(m.spinner.View() + " Checking available authentication…")

	case connectPhaseAllow:
		name := backendDisplayName(m.result.Backend)
		sb.WriteString(fmt.Sprintf(connectAllowQuestion, name))
		sb.WriteString(lipgloss.NewStyle().Foreground(mutedColor).
			Render("via Agent Client Protocol (ACP)"))
		sb.WriteString("\n\n")
		sb.WriteString("Authentication is checked only after you allow this harness.\n\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("y/enter allow · n/esc cancel"))

	case connectPhaseReady:
		sb.WriteString(lipgloss.NewStyle().Foreground(successColor).
			Render("✓ Allowed " + backendDisplayName(m.result.Backend)))
		sb.WriteString("\n\nUsing " + m.result.ProviderName)
		sb.WriteString("\n\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("press any key to dismiss"))

	case connectPhaseError:
		sb.WriteString(lipgloss.NewStyle().Foreground(dangerColor).
			Render("Error: " + m.errMsg))
		sb.WriteString("\n\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("press any key to dismiss"))

	case connectPhasePickBackend:
		sb.WriteString(lipgloss.NewStyle().Foreground(mutedColor).
			Render(connectPickerQuestion))
		sb.WriteString("\n\n")
		for i, row := range m.backends {
			prefix := "  "
			labelStyle := lipgloss.NewStyle().Foreground(textColor)
			if i == m.cursor {
				prefix = "▶ "
				labelStyle = labelStyle.Bold(true).Background(primaryColor)
			}
			status := lipgloss.NewStyle().Foreground(primaryColor).Render(connectNeedsApprovalLabel)
			if row.IsAllowed && row.ProviderName == "" {
				status = lipgloss.NewStyle().Foreground(dimColor).Render("configure auth")
			} else if row.IsAllowed {
				status = lipgloss.NewStyle().Foreground(successColor).Render("✓ " + row.ProviderName)
			}
			line := fmt.Sprintf("%s%s", prefix, labelStyle.Render(backendDisplayName(row.Backend)))
			gap := max(innerWidth-lipgloss.Width(line)-lipgloss.Width(status)-2, 1)
			sb.WriteString(line + strings.Repeat(" ", gap) + status)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render(connectPickerHelp))
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())
}
