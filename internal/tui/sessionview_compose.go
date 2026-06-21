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
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
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
//
// The gitRef is built from projectDir (LocalPath) plus the worktree ID
// cached by an earlier `clank push` (read via agent.ReadLocalWorktreeID),
// so the background fetchAgents/fetchModels prefetch can target it.
// Until a push has populated the cache the ref is local-only and any
// cross-host operations will fail at launch.
func NewSessionViewComposing(client *daemonclient.Client, projectDir string) *SessionViewModel {
	// Default backend: prefer the user's saved choice, falling back to
	// agent.DefaultBackend. Errors (corrupt prefs) silently fall back —
	// the picker UI will show the resolved choice and the user can
	// toggle from there.
	prefs, _ := config.LoadPreferences()
	defaultBackend, _ := agent.ResolveBackendPreference(prefs.DefaultBackend)
	return newSessionViewComposingWithBackend(client, projectDir, defaultBackend)
}

// newSessionViewComposingWithBackend builds the composing model with an
// explicit default backend instead of resolving it from saved preferences.
// NewSessionViewComposing wraps it with the preference lookup; tests call it
// directly to pin the backend so the developer's on-disk DefaultBackend
// preference can't leak into compose-behaviour assertions.
func newSessionViewComposingWithBackend(client *daemonclient.Client, projectDir string, defaultBackend agent.BackendType) *SessionViewModel {
	ta := newPromptTextarea("Describe the task for the agent...", 5)
	ta.Focus()
	sp := spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(successColor)),
	)
	// Send LocalPath (laptop-local host uses it directly) plus the
	// cached worktree ID (cross-host stable identity, set by
	// `clank push`). See agent.GitRef godoc.
	ref := agent.GitRef{LocalPath: projectDir}
	if id, _ := agent.ReadLocalWorktreeID(projectDir); id != "" {
		ref.WorktreeID = id
	}
	m := &SessionViewModel{
		client:      client,
		composing:   true,
		inputActive: true,
		backend:     defaultBackend,
		projectDir:  projectDir,
		hostname:    host.HostLocal,
		gitRef:      ref,
		follow:      true,
		input:       ta,
		spinner:     sp,
	}
	// Claude's modes are a static set seeded up front; OpenCode's agents arrive
	// asynchronously via fetchAgents (Init).
	if defaultBackend == agent.BackendClaudeCode {
		m.modes, m.selectedMode = claudePermissionModes()
	}
	return m
}

// updateCompose handles all messages while in composing mode.
func (m *SessionViewModel) updateCompose(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Model picker takes priority when open.
	if m.showModelPicker {
		switch msg := msg.(type) {
		case modelPickerResultMsg:
			m.showModelPicker = false
			m.selectedModel = msg.selectedModel
			backend, pref := m.snapshotModelPreference()
			go persistModelPreference(backend, pref)
			return m, m.input.Focus()
		case modelPickerCancelMsg:
			m.showModelPicker = false
			return m, m.input.Focus()
		case modelPickerConnectProviderMsg:
			m.showModelPicker = false
			backend := m.backend
			return m, func() tea.Msg { return openProviderAuthFromSessionMsg{backend: backend} }
		default:
			var cmd tea.Cmd
			m.modelPicker, cmd = m.modelPicker.Update(msg)
			return m, cmd
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(m.width - promptInputBorderSize)
		return m, nil

	case agentsResultMsg:
		// OpenCode only — Claude seeds its static modes synchronously.
		m.modes, m.selectedMode = agentSelectableModes(msg.agents, "")
		return m, nil

	case modelsResultMsg:
		m.models = msg.models
		m.selectedModel = -1 // default: no override

		// Restore the user's preferred model for this backend.
		// Per-backend so a github-copilot model picked under opencode
		// doesn't leak into a claude-code session and crash the CLI
		// with `--model claude-opus-4.7` (or any other id not in the
		// claude-code closed enum).
		prefs, _ := config.LoadPreferences()
		pref := prefs.ModelFor(string(m.backend))
		if !pref.IsZero() {
			for i, model := range m.models {
				if model.ID == pref.ModelID && model.ProviderID == pref.ProviderID {
					m.selectedModel = i
					break
				}
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
		// Compose is an overlay over whatever the right pane was
		// previously showing — Esc restores that state, it doesn't
		// route to the inbox screen.
		return m, func() tea.Msg { return closeComposeMsg{} }

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+b"))):
		// Toggle backend.
		if m.backend == agent.BackendOpenCode {
			m.backend = agent.BackendClaudeCode
		} else {
			m.backend = agent.BackendOpenCode
		}
		// Each backend has its own model catalog (claude-code is a
		// closed sonnet/opus/haiku enum; opencode aggregates whatever
		// providers the user has configured). Drop the stale list and
		// refetch — leaving it would let the user submit the previous
		// backend's selectedModel index, which we'd then forward as
		// `--model <opencode-id>` to the claude CLI and crash on
		// spawn. Reset selectedModel too so the new fetch's
		// preference-restore can re-resolve it from disk per backend.
		m.models = nil
		m.selectedModel = -1
		// Rebuild the mode list for the new backend: Claude's modes are static;
		// OpenCode's agents are fetched.
		if m.backend == agent.BackendClaudeCode {
			m.modes, m.selectedMode = claudePermissionModes()
			return m, m.fetchModels()
		}
		m.modes, m.selectedMode = nil, 0
		return m, tea.Batch(m.fetchAgents(), m.fetchModels())

	case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
		// Cycle through modes (OpenCode agents or Claude permission modes).
		if len(m.modes) > 1 {
			m.selectedMode = (m.selectedMode + 1) % len(m.modes)
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))):
		// Open model picker modal.
		if len(m.models) > 0 {
			m.showModelPicker = true
			m.modelPicker = newModelPicker(m.models, m.selectedModel, m.backend)
		}
		return m, m.modelPicker.Init()

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

	// LocalPath for the laptop-local host; WorktreeID for cross-host
	// targeting once `clank sync push` has registered the worktree.
	gitRef := agent.GitRef{
		LocalPath:      m.projectDir,
		WorktreeBranch: m.worktreeBranch,
	}
	if id, _ := agent.ReadLocalWorktreeID(m.projectDir); id != "" {
		gitRef.WorktreeID = id
	}

	req := agent.StartRequest{
		Backend:  m.backend,
		Hostname: host.HostLocal,
		GitRef:   gitRef,
		Prompt:   prompt,
	}
	if len(m.modes) > 0 {
		sel := m.modes[m.selectedMode]
		req.Agent = sel.agent
		req.PermissionMode = sel.perm
	}
	if m.selectedModel >= 0 && m.selectedModel < len(m.models) {
		model := m.models[m.selectedModel]
		req.Model = &agent.ModelOverride{
			ModelID:    model.ID,
			ProviderID: model.ProviderID,
		}
	}

	return m, m.createSessionCmd(req)
}

// createSessionCmd subscribes to SSE first, then creates the session.
// This avoids the race where events are emitted before the TUI subscribes.
func (m *SessionViewModel) createSessionCmd(req agent.StartRequest) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		sseCtx, sseCancel := context.WithCancel(context.Background())
		events, err := client.Sessions().Subscribe(sseCtx)
		if err != nil {
			sseCancel()
			return sessionCreateResultMsg{err: fmt.Errorf("subscribe events: %w", err)}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		info, err := client.Sessions().Create(ctx, req)
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
	modeLabel := ""
	if len(m.modes) > 0 {
		modeLabel = m.modes[m.selectedMode].label
	}
	m.entries = append(m.entries, displayEntry{
		kind:    entryUser,
		content: prompt,
		agent:   modeLabel,
	})

	// Reset the textarea for follow-up messages.
	m.input = newPromptTextarea("Type a follow-up message...", 3)
	if m.width > 0 {
		m.input.SetWidth(m.width - promptInputBorderSize)
	}

	// Start reading events + fetch session info + start spinner +
	// notify the inbox that the compose transitioned to a live
	// session so its tracking (activeConnID, sidebar rail, last-
	// session-by-cwd) stays in sync.
	newID := m.sessionID
	return m, tea.Batch(
		m.fetchSessionInfo(),
		waitForEvent(m.eventsCh),
		m.spinner.Tick,
		func() tea.Msg { return composeSubmittedMsg{sessionID: newID} },
	)
}

// viewCompose renders the composing mode screen.
func (m *SessionViewModel) viewCompose() tea.View {
	if m.width == 0 {
		v := newVoiceEnabledView("Loading...")
		return v
	}

	var sb strings.Builder

	// Header. Prepend a blank row so the title lines up with the
	// sidebar's "Worktrees" header (one row below the sidebar's top
	// border). The compose view has no outer border of its own, so
	// without this nudge the title would render flush against the
	// top edge.
	sb.WriteString("\n")
	sb.WriteString(m.renderComposeHeader())
	sb.WriteString("\n\n")

	// Error banner.
	if m.err != nil {
		sb.WriteString(renderError(m.err, m.width))
		sb.WriteString("\n\n")
	}

	// Backend selector.
	sb.WriteString(m.renderBackendSelector())
	sb.WriteString("\n")

	// Project directory.
	labelSty := lipgloss.NewStyle().Foreground(dimColor).Width(12)
	sb.WriteString("  " + labelSty.Render("Project:"))
	sb.WriteString(lipgloss.NewStyle().Foreground(textColor).Render(m.projectDir))
	sb.WriteString("\n")

	// Worktree branch (if selected).
	if m.worktreeBranch != "" {
		sb.WriteString("  " + labelSty.Render("Branch:"))
		sb.WriteString(lipgloss.NewStyle().Foreground(secondaryColor).Render(m.worktreeBranch))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// Prompt textarea with integrated mode badge.
	sb.WriteString(m.renderPromptBox())
	sb.WriteString("\n\n")

	// Help bar.
	qLabel := "esc: back"
	if m.standalone {
		qLabel = "esc: quit"
	}
	helpParts := []string{"enter: launch", "shift+enter: newline", "ctrl+b: toggle backend"}
	if len(m.modes) > 1 {
		helpParts = append(helpParts, "tab: cycle mode")
	}
	if m.backend == agent.BackendOpenCode && len(m.models) > 0 {
		helpParts = append(helpParts, "shift+tab: select model")
	}
	helpParts = append(helpParts, qLabel)
	help := helpStyle.Render(strings.Join(helpParts, " | "))
	sb.WriteString(help)

	output := sb.String()
	output = m.overlayModelPicker(output)
	v := newVoiceEnabledView(output)
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
	header := title + strings.Repeat(" ", gap) + backendStr

	if m.isNewWorktree {
		indicator := "  New Worktree"
		if m.baseBranch != "" {
			indicator += " | base: " + m.baseBranch
		}
		header += "\n" + lipgloss.NewStyle().Foreground(secondaryColor).Render(indicator)
	}

	return header
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
	// Border color follows focus: muted when blurred so it doesn't
	// compete visually with other highlighted UI (e.g. permission prompt).
	// The mode badge always uses the agent color for identification.
	modeBadge := ""
	bc := mutedColor

	if len(m.modes) > 0 {
		sel := m.modes[m.selectedMode]
		mc := modeColor(sel)
		modeBadge = lipgloss.NewStyle().Foreground(mc).Bold(true).Render(sel.label)
		if m.input.Focused() {
			bc = mc
		}
	} else if m.info != nil && m.info.Agent != "" {
		// Modes not loaded yet — fall back to session info's agent name for the
		// correct color, mirroring the fallback in renderHeader().
		mc := agentColor(m.info.Agent)
		modeBadge = lipgloss.NewStyle().Foreground(mc).Bold(true).Render(m.info.Agent)
		if m.input.Focused() {
			bc = mc
		}
	} else if m.input.Focused() {
		bc = primaryColor
	}

	// Model badge (shown after mode badge when a model override is selected).
	modelBadge := ""
	if m.selectedModel >= 0 && m.selectedModel < len(m.models) {
		model := m.models[m.selectedModel]
		modelBadge = lipgloss.NewStyle().Foreground(secondaryColor).Render(model.ProviderID + "/" + model.ID)
	}

	// Double-tap ctrl+c hint (shown briefly after first press).
	ctrlCHint := ""
	if !m.lastCtrlC.IsZero() && time.Since(m.lastCtrlC) < time.Second {
		ctrlCHint = lipgloss.NewStyle().Foreground(warningColor).Render("press ctrl+c again to quit")
	}

	// Build inner content: badge line (with optional hint) + textarea.
	var inner strings.Builder
	innerWidth := m.width - promptInputBorderSize

	// Combine mode badge and model badge.
	combinedBadge := modeBadge
	if modelBadge != "" {
		if combinedBadge != "" {
			combinedBadge += " " + modelBadge
		} else {
			combinedBadge = modelBadge
		}
	}

	if combinedBadge != "" || ctrlCHint != "" {
		badgeWidth := lipgloss.Width(combinedBadge)
		hintWidth := lipgloss.Width(ctrlCHint)
		gap := innerWidth - badgeWidth - hintWidth
		if gap < 1 {
			gap = 1
		}
		inner.WriteString(combinedBadge + strings.Repeat(" ", gap) + ctrlCHint)
		inner.WriteString("\n")
	}
	inner.WriteString(m.input.View())

	return promptInputStyleWithColor(bc).Render(inner.String())
}
