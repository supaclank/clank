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
	return &SessionViewModel{
		client:      client,
		composing:   true,
		inputActive: true,
		backend:     agent.BackendOpenCode,
		projectDir:  projectDir,
		follow:      true,
		input:       ta,
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

	case tea.KeyPressMsg:
		return m.handleComposeKey(msg)
	}

	// Forward everything else to the textarea.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *SessionViewModel) handleComposeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		return m, tea.Quit

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
	}

	// Forward to textarea.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// launchSession validates the prompt, subscribes to SSE, and creates the session.
func (m *SessionViewModel) launchSession() (tea.Model, tea.Cmd) {
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
	m.entries = append(m.entries, displayEntry{
		kind:    entryUser,
		content: prompt,
	})

	// Reset the textarea for follow-up messages.
	m.input = newPromptTextarea("Type a follow-up message...", 3)
	if m.width > 0 {
		m.input.SetWidth(m.width - promptInputBorderSize)
	}

	// Start reading events + fetch session info.
	return m, tea.Batch(
		m.fetchSessionInfo(),
		waitForEvent(m.eventsCh, m.sessionID),
	)
}

// viewCompose renders the composing mode screen.
func (m *SessionViewModel) viewCompose() tea.View {
	if m.width == 0 {
		v := tea.NewView("Loading...")
		v.AltScreen = true
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

	v := tea.NewView(sb.String())
	v.AltScreen = true
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
	}

	// Build inner content: mode badge line + textarea.
	var inner strings.Builder
	if modeBadge != "" {
		inner.WriteString(modeBadge)
		inner.WriteString("\n")
	}
	inner.WriteString(m.input.View())

	return promptInputStyleWithColor(bc).Render(inner.String())
}
