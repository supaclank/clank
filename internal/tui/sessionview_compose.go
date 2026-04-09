package tui

// Composing mode for SessionViewModel: the user types their first prompt
// before any daemon session exists. On send, the session is created and
// the view transitions to the normal streaming session view.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemon"
)

// sessionCreateResultMsg carries the result of creating a session from composing mode.
type sessionCreateResultMsg struct {
	sessionID string
	events    <-chan agent.Event
	cancel    context.CancelFunc
	err       error
}

// NewSessionViewComposing creates a SessionViewModel in composing mode.
// No daemon session exists yet — the user writes their first prompt here.
func NewSessionViewComposing(client *daemon.Client, projectDir string) *SessionViewModel {
	ta := newPromptTextarea("Describe the task for the agent...", 5)
	ta.Focus()
	sp := spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(successColor)),
	)
	return &SessionViewModel{
		client:      client,
		composing:   true,
		inputActive: true,
		backend:     agent.BackendOpenCode,
		projectDir:  projectDir,
		follow:      true,
		input:       ta,
		spinner:     sp,
	}
}

// updateCompose handles all messages while in composing mode.
func (m *SessionViewModel) updateCompose(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(m.width - promptInputBorderSize)
		return m, nil

	case agentsResultMsg:
		m.agents = msg.agents
		// Default to "build" if present.
		for i, a := range m.agents {
			if a.Name == "build" {
				m.selectedAgent = i
				break
			}
		}
		return m, nil

	case sessionCreateResultMsg:
		return m.handleCreateResult(msg)

	case clearCtrlCHintMsg:
		m.lastCtrlC = time.Time{}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleComposeKey(msg)
	}

	// Forward everything else to the textarea.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *SessionViewModel) handleComposeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	msg = normalizeKeyCase(msg)

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		return m.handleCtrlCQuit()

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		if m.standalone {
			return m, tea.Quit
		}
		return m, func() tea.Msg { return backToInboxMsg{} }

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+b"))):
		// Toggle backend.
		if m.backend == agent.BackendOpenCode {
			m.backend = agent.BackendClaudeCode
		} else {
			m.backend = agent.BackendOpenCode
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
		// Cycle through agents (only when agents are loaded).
		if len(m.agents) > 1 {
			m.selectedAgent = (m.selectedAgent + 1) % len(m.agents)
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		// Send prompt — shift+enter inserts newline (handled by textarea keybinding).
		return m.launchSession()

	case key.Matches(msg, wordBackwardBinding):
		// Workaround: upstream bubbles textarea.wordLeft() has an
		// unconditional for{} loop that never terminates when the cursor
		// is at (0,0) — the empty-input case. Intercept and no-op here
		// to prevent an infinite loop that freezes the entire UI.
		// See: https://github.com/charmbracelet/bubbles/issues/XXX
		if wordLeftWouldHang(m.input) {
			return m, nil
		}
	}

	// Forward to textarea.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// launchSession validates the prompt, subscribes to SSE, and creates the session.
func (m *SessionViewModel) launchSession() (tea.Model, tea.Cmd) {
	if m.submitting {
		return m, nil // Already in flight — ignore duplicate Enter.
	}

	prompt := strings.TrimSpace(m.input.Value())
	if prompt == "" {
		m.err = fmt.Errorf("prompt is required")
		return m, nil
	}
	if m.projectDir == "" {
		m.err = fmt.Errorf("project directory is required")
		return m, nil
	}

	m.err = nil
	m.submitting = true

	req := agent.StartRequest{
		Backend:    m.backend,
		ProjectDir: m.projectDir,
		Prompt:     prompt,
	}
	if len(m.agents) > 0 {
		req.Agent = m.agents[m.selectedAgent].Name
	}

	return m, m.createSessionCmd(req)
}

// createSessionCmd subscribes to SSE first, then creates the session.
// This avoids the race where events are emitted before the TUI subscribes.
func (m *SessionViewModel) createSessionCmd(req agent.StartRequest) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		sseCtx, sseCancel := context.WithCancel(context.Background())
		events, err := client.SubscribeEvents(sseCtx)
		if err != nil {
			sseCancel()
			return sessionCreateResultMsg{err: fmt.Errorf("subscribe events: %w", err)}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		info, err := client.CreateSession(ctx, req)
		if err != nil {
			sseCancel()
			return sessionCreateResultMsg{err: fmt.Errorf("create session: %w", err)}
		}

		return sessionCreateResultMsg{
			sessionID: info.ID,
			events:    events,
			cancel:    sseCancel,
		}
	}
}

// handleCreateResult transitions from composing mode to the normal session view.
func (m *SessionViewModel) handleCreateResult(msg sessionCreateResultMsg) (tea.Model, tea.Cmd) {
	m.submitting = false
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}

	// Transition to normal session mode.
	prompt := strings.TrimSpace(m.input.Value())
	m.composing = false
	m.sessionID = msg.sessionID
	m.eventsCh = msg.events
	m.cancelEvents = msg.cancel
	m.inputActive = false
	m.input.Blur()
	m.input.Reset()

	// Show the user's prompt as the first entry.
	agentName := ""
	if len(m.agents) > 0 {
		agentName = m.agents[m.selectedAgent].Name
	}
	m.entries = append(m.entries, displayEntry{
		kind:    entryUser,
		content: prompt,
		agent:   agentName,
	})

	// Reset the textarea for follow-up messages.
	m.input = newPromptTextarea("Type a follow-up message...", 3)
	if m.width > 0 {
		m.input.SetWidth(m.width - promptInputBorderSize)
	}

	// Start reading events + fetch session info + start spinner.
	return m, tea.Batch(
		m.fetchSessionInfo(),
		waitForEvent(m.eventsCh, m.sessionID),
		m.spinner.Tick,
	)
}

// viewCompose renders the composing mode screen.
func (m *SessionViewModel) viewCompose() tea.View {
	if m.width == 0 {
		v := newVoiceEnabledView("Loading...")
		return v
	}

	var sb strings.Builder

	// Header.
	sb.WriteString(m.renderComposeHeader())
	sb.WriteString("\n\n")

	// Error banner.
	if m.err != nil {
		errMsg := lipgloss.NewStyle().Foreground(dangerColor).Render(fmt.Sprintf("Error: %v", m.err))
		sb.WriteString(errMsg)
		sb.WriteString("\n\n")
	}

	// Backend selector.
	sb.WriteString(m.renderBackendSelector())
	sb.WriteString("\n")

	// Project directory.
	labelSty := lipgloss.NewStyle().Foreground(dimColor).Width(12)
	sb.WriteString("  " + labelSty.Render("Project:"))
	sb.WriteString(lipgloss.NewStyle().Foreground(textColor).Render(m.projectDir))
	sb.WriteString("\n\n")

	// Prompt textarea with integrated mode badge.
	sb.WriteString(m.renderPromptBox())
	sb.WriteString("\n\n")

	// Help bar.
	qLabel := "esc: back"
	if m.standalone {
		qLabel = "esc: quit"
	}
	helpParts := []string{"enter: launch", "shift+enter: newline", "ctrl+b: toggle backend"}
	if m.backend == agent.BackendOpenCode && len(m.agents) > 1 {
		helpParts = append(helpParts, "tab: cycle mode")
	}
	helpParts = append(helpParts, qLabel)
	help := helpStyle.Render(strings.Join(helpParts, " | "))
	sb.WriteString(help)

	v := newVoiceEnabledView(sb.String())
	return v
}

func (m *SessionViewModel) renderComposeHeader() string {
	title := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render("New Session")

	backendStr := lipgloss.NewStyle().Foreground(dimColor).Render("[" + string(m.backend) + "]")
	gap := m.width - lipgloss.Width(title) - lipgloss.Width(backendStr)
	if gap < 2 {
		gap = 2
	}
	return title + strings.Repeat(" ", gap) + backendStr
}

func (m *SessionViewModel) renderBackendSelector() string {
	labelSty := lipgloss.NewStyle().Foreground(dimColor).Width(12)
	label := labelSty.Render("Backend:")

	ocStyle := lipgloss.NewStyle().Foreground(dimColor)
	ccStyle := lipgloss.NewStyle().Foreground(dimColor)
	if m.backend == agent.BackendOpenCode {
		ocStyle = lipgloss.NewStyle().Foreground(successColor).Bold(true)
	} else {
		ccStyle = lipgloss.NewStyle().Foreground(successColor).Bold(true)
	}

	return fmt.Sprintf("  %s[%s]  [%s]",
		label,
		ocStyle.Render("OpenCode"),
		ccStyle.Render("Claude Code"),
	)
}

// renderPromptBox renders the prompt textarea with an integrated mode badge
// inside the border. The border color matches the current agent mode.
func (m *SessionViewModel) renderPromptBox() string {
	// Determine mode badge and border color.
	modeBadge := ""
	bc := mutedColor // default border color when unfocused
	if m.input.Focused() {
		bc = primaryColor
	}

	if len(m.agents) > 0 {
		agentName := m.agents[m.selectedAgent].Name
		mc := agentColor(agentName)
		bc = mc
		modeBadge = lipgloss.NewStyle().Foreground(mc).Bold(true).Render(agentName)
	} else if m.info != nil && m.info.Agent != "" {
		// Agents list not loaded yet (or not available for this backend).
		// Fall back to session info's agent name for the correct color,
		// mirroring the fallback in renderHeader().
		mc := agentColor(m.info.Agent)
		bc = mc
		modeBadge = lipgloss.NewStyle().Foreground(mc).Bold(true).Render(m.info.Agent)
	}

	// Double-tap ctrl+c hint (shown briefly after first press).
	ctrlCHint := ""
	if !m.lastCtrlC.IsZero() && time.Since(m.lastCtrlC) < time.Second {
		ctrlCHint = lipgloss.NewStyle().Foreground(warningColor).Render("press ctrl+c again to quit")
	}

	// Build inner content: badge line (with optional hint) + textarea.
	var inner strings.Builder
	innerWidth := m.width - promptInputBorderSize
	if modeBadge != "" || ctrlCHint != "" {
		badgeWidth := lipgloss.Width(modeBadge)
		hintWidth := lipgloss.Width(ctrlCHint)
		gap := innerWidth - badgeWidth - hintWidth
		if gap < 1 {
			gap = 1
		}
		inner.WriteString(modeBadge + strings.Repeat(" ", gap) + ctrlCHint)
		inner.WriteString("\n")
	}
	inner.WriteString(m.input.View())

	return promptInputStyleWithColor(bc).Render(inner.String())
}
