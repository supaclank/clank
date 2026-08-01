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
//   pickBackend → provider auth (list → confirm → … → success)
//
// A ConnectModel built with a backend skips the picker entirely.

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
)

// ConnectResult is what the CLI reads off the model after the program
// exits. Backend is set as soon as the user picks one, so a canceled
// run still reports what they were connecting.
type ConnectResult struct {
	Backend     agent.BackendType
	IsConnected bool
}

// connectPhase tracks which of the two stages the model is in. The
// provider phase delegates every message to providerAuthModel.
type connectPhase int

const (
	// connectPhaseLoading fetches the unfiltered provider catalog that
	// the backend picker renders connection state from.
	connectPhaseLoading connectPhase = iota
	connectPhasePickBackend
	connectPhaseProvider
	connectPhaseError
)

// connectProvidersLoadedMsg carries the picker's own catalog read.
// Distinct from providerListLoadedMsg so a late arrival can never be
// mistaken for the hosted model's list load.
type connectProvidersLoadedMsg struct {
	providers []agent.ProviderAuthInfo
	err       error
}

// ConnectModel drives `clank connect` end to end.
type ConnectModel struct {
	caller ProviderAuthCaller

	phase  connectPhase
	errMsg string

	// backends is the picker's row set: every backend clank can launch,
	// paired with whether it already has a connected provider.
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
	Backend     agent.BackendType
	IsConnected bool
}

// NewConnectModel returns the connect program. A non-empty backend
// jumps straight into that backend's provider flow; an empty one shows
// the backend picker first.
func NewConnectModel(caller ProviderAuthCaller, backend agent.BackendType) *ConnectModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(primaryColor)

	m := &ConnectModel{caller: caller, spinner: sp}
	if backend != "" {
		m.phase = connectPhaseProvider
		m.result.Backend = backend
		m.providerAuth = newProviderAuthModel(caller, backend, "")
		return m
	}
	m.phase = connectPhaseLoading
	m.hasPicker = true
	return m
}

// Result reports what the run achieved. Read after the program exits.
//
// IsConnected comes from the auth flow's own terminal state, not from
// the message that dismisses it: the credential is written before the
// success screen appears, so quitting at that screen instead of pressing
// a key still connected the provider.
func (m *ConnectModel) Result() ConnectResult {
	result := m.result
	result.IsConnected = m.providerAuth.phase == providerPhaseSuccess
	return result
}

func (m *ConnectModel) Init() tea.Cmd {
	if m.phase == connectPhaseProvider {
		return m.providerAuth.Init()
	}
	return tea.Batch(m.spinner.Tick, m.loadBackendsCmd())
}

func (m *ConnectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case providerAuthDoneMsg:
		// Result() reads what was achieved off the flow's own phase.
		return m, tea.Quit

	case providerAuthCancelMsg:
		// Leaving the provider list steps back to the backend picker
		// when there is one — a user who picked the wrong agent wants
		// the question again, not the program gone.
		if m.hasPicker && m.phase == connectPhaseProvider {
			m.phase = connectPhasePickBackend
			m.providerAuth = providerAuthModel{}
			m.result.Backend = ""
			return m, nil
		}
		return m, tea.Quit

	case connectProvidersLoadedMsg:
		if msg.err != nil {
			m.phase = connectPhaseError
			m.errMsg = msg.err.Error()
			return m, nil
		}
		m.backends = connectBackendRows(msg.providers)
		m.phase = connectPhasePickBackend
		return m, nil

	case tea.KeyPressMsg:
		// ctrl+c is the root program's business — the inbox owned it
		// before, and providerAuthModel only knows esc.
		if key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))) {
			return m, tea.Quit
		}
		if m.phase != connectPhaseProvider {
			return m.handleKey(msg)
		}
	}

	if m.phase == connectPhaseProvider {
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

func (m *ConnectModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	msg = normalizeKeyCase(msg)
	if key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q"))) {
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
				backend := m.backends[m.cursor].Backend
				m.result.Backend = backend
				m.phase = connectPhaseProvider
				m.providerAuth = newProviderAuthModel(m.caller, backend, "")
				return m, m.providerAuth.Init()
			}
		}
	}
	return m, nil
}

func (m *ConnectModel) loadBackendsCmd() tea.Cmd {
	caller := m.caller
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), providerListLoadTimeout)
		defer cancel()
		// Unfiltered: the picker needs every backend's state at once.
		providers, err := caller.ListAuthProviders(ctx, "")
		return connectProvidersLoadedMsg{providers: providers, err: err}
	}
}

// connectBackendRows pairs every launchable backend with its connection
// state. Backends absent from the catalog list as not connected rather
// than disappearing — "clank can run this, you just haven't connected
// it" is exactly what the picker exists to say.
func connectBackendRows(providers []agent.ProviderAuthInfo) []connectBackendRow {
	rows := make([]connectBackendRow, 0, len(agent.AllBackends))
	for _, bt := range agent.AllBackends {
		rows = append(rows, connectBackendRow{
			Backend:     bt,
			IsConnected: agent.IsBackendConnected(providers, bt),
		})
	}
	return rows
}

// View renders inline (no alt screen) so the connect flow stays in the
// terminal's scrollback alongside whatever ran before it — `clank
// preview` prints around this.
func (m *ConnectModel) View() tea.View {
	return connectView(m.body())
}

// connectView wraps rendered body text in the inline, mouse-free
// tea.View both phases share.
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
		Width(innerWidth).Render("Connect an agent"))
	sb.WriteString("\n\n")

	switch m.phase {
	case connectPhaseLoading:
		sb.WriteString(m.spinner.View())
		sb.WriteString(" Checking which agents are connected…")

	case connectPhaseError:
		sb.WriteString(lipgloss.NewStyle().Foreground(dangerColor).
			Render("Error: " + m.errMsg))
		sb.WriteString("\n\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("press any key to dismiss"))

	case connectPhasePickBackend:
		sb.WriteString(lipgloss.NewStyle().Foreground(mutedColor).
			Render("Which agent do you want to use?"))
		sb.WriteString("\n\n")
		for i, row := range m.backends {
			prefix := "  "
			labelStyle := lipgloss.NewStyle().Foreground(textColor)
			if i == m.cursor {
				prefix = "▶ "
				labelStyle = labelStyle.Bold(true).Background(primaryColor)
			}
			status := lipgloss.NewStyle().Foreground(dimColor).Render("not connected")
			if row.IsConnected {
				status = lipgloss.NewStyle().Foreground(successColor).Render("✓ connected")
			}
			line := fmt.Sprintf("%s%s", prefix, labelStyle.Render(backendDisplayName(row.Backend)))
			gap := max(innerWidth-lipgloss.Width(line)-lipgloss.Width(status)-2, 1)
			sb.WriteString(line + strings.Repeat(" ", gap) + status)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Render("↑↓ navigate · enter select · esc cancel"))
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())
}
