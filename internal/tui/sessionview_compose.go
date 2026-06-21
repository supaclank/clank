package tui

// Composing mode for SessionViewModel: the user types their first prompt
// before any daemon session exists. On send, the session is created and
// the view transitions to the normal streaming session view.

import (
	"context"
	"fmt"
	"path/filepath"
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
	"github.com/acksell/clank/internal/host/petname"
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

	// Folder picker takes priority when open.
	if m.showFolderPicker {
		switch msg := msg.(type) {
		case folderPickerResultMsg:
			m.showFolderPicker = false
			return m, m.applyProjectFolder(msg.dir, focusFolder)
		case folderPickerCancelMsg:
			// Stay on the Project row (textarea stays blurred).
			m.showFolderPicker = false
			return m, nil
		default:
			var cmd tea.Cmd
			m.folderPicker, cmd = m.folderPicker.Update(msg)
			return m, cmd
		}
	}

	// Worktree picker takes priority when open.
	if m.showWorktreePicker {
		switch msg := msg.(type) {
		case worktreePickerResultMsg:
			m.showWorktreePicker = false
			return m, m.applyProjectFolder(msg.dir, focusWorktree)
		case worktreePickerCancelMsg:
			m.showWorktreePicker = false
			return m, nil
		default:
			var cmd tea.Cmd
			m.worktreePicker, cmd = m.worktreePicker.Update(msg)
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
		// Esc exits compose from any row. (An open picker handles its own
		// Esc first via the picker blocks in updateCompose, so this only
		// fires when no picker is open.)
		if m.standalone {
			return m, tea.Quit
		}
		// Compose is an overlay over whatever the right pane was
		// previously showing — Esc restores that state, it doesn't
		// route to the inbox screen.
		return m, func() tea.Msg { return closeComposeMsg{} }

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+b"))):
		return m, m.toggleBackend()
	}

	// When a non-prompt row is focused, the arrow keys drive navigation
	// and per-row actions rather than editing the prompt.
	if m.composeFocus != focusPrompt {
		return m.handleComposeNavKey(msg)
	}

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("up"))):
		// Escape upward to the row above only when the cursor is already
		// on the first line; otherwise let the textarea move the cursor.
		if m.input.Line() == 0 {
			return m, m.moveComposeFocus(-1)
		}

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
	//
	gitRef := agent.GitRef{
		LocalPath:      m.projectDir,
		WorktreeBranch: m.effectiveWorktreeBranch(),
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

	// Compose fields as borderless "label: value" rows; the chevron cursor and
	// recolored label mark the focused row without layout shift.
	fields := []composeFieldSpec{
		{label: "Backend", value: m.backendValue(m.composeFocus == focusBackend), focus: focusBackend},
		{label: "Project", value: lipgloss.NewStyle().Foreground(textColor).Render(m.projectDir), focus: focusFolder},
		{label: "Worktree", value: m.worktreeValue(), focus: focusWorktree},
		{label: "New worktree", value: m.newWorktreeValue(), focus: focusNewWorktree},
	}
	sb.WriteString(m.renderComposeFields(fields))
	sb.WriteString("\n\n")

	// Prompt textarea with integrated mode badge.
	sb.WriteString(m.renderPromptBox())
	sb.WriteString("\n\n")

	// Help bar — context-sensitive to the focused row.
	qLabel := "esc: back"
	if m.standalone {
		qLabel = "esc: quit"
	}
	var helpParts []string
	switch m.composeFocus {
	case focusBackend:
		helpParts = []string{"←→: choose", "enter: switch", "↑↓: navigate", qLabel}
	case focusFolder:
		helpParts = []string{"enter: choose folder", "↑↓: navigate", qLabel}
	case focusWorktree:
		helpParts = []string{"enter: choose worktree", "↑↓: navigate", qLabel}
	case focusNewWorktree:
		helpParts = []string{"enter: toggle", "↑↓: navigate", qLabel}
	default: // focusPrompt
		helpParts = []string{"↑: fields", "enter: launch", "shift+enter: newline", "ctrl+b: toggle backend"}
		if len(m.modes) > 1 {
			helpParts = append(helpParts, "tab: cycle mode")
		}
		if m.backend == agent.BackendOpenCode && len(m.models) > 0 {
			helpParts = append(helpParts, "shift+tab: select model")
		}
		helpParts = append(helpParts, qLabel)
	}
	help := helpStyle.Render(strings.Join(helpParts, " | "))
	sb.WriteString(help)

	output := sb.String()
	output = m.overlayModelPicker(output)
	output = m.overlayFolderPicker(output)
	output = m.overlayWorktreePicker(output)
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

	return header
}

// backendValue renders the two backend choices on two independent visual
// channels: bracket SHAPE encodes selection (the active backend clamps tight
// "[x]"; the others sit open "[ x ]"), while bracket COLOR encodes the
// cursor (vibrant on the ←/→-hovered option). Enter commits the hovered
// option, so the tight clamp and green text move to it.
//
// The brackets occupy a fixed 2-cell gutter on each side of the text: tight
// brackets sit on the inner cell, open brackets on the outer cell. The text
// stays in the same column either way — only the bracket glyph shifts.
func (m *SessionViewModel) backendValue(focused bool) string {
	option := func(name string, idx int, active bool) string {
		textColor := dimColor
		if active {
			textColor = successColor
		}
		text := lipgloss.NewStyle().Foreground(textColor).Bold(active).Render(name)

		// Inner cell (tight) when selected; outer cell (open) otherwise.
		lb, rb := "[ ", " ]" // open
		if active {
			lb, rb = " [", "] " // tight
		}

		bracketColor := dimColor
		hovered := focused && m.backendCursor == idx
		if hovered {
			bracketColor = primaryColor
		}
		bracket := lipgloss.NewStyle().Foreground(bracketColor).Bold(hovered)
		return bracket.Render(lb) + text + bracket.Render(rb)
	}
	oc := option("OpenCode", 0, m.backend == agent.BackendOpenCode)
	cc := option("Claude Code", 1, m.backend == agent.BackendClaudeCode)
	return oc + "  " + cc
}

// newWorktreeValue renders the New-worktree toggle's current state.
func (m *SessionViewModel) newWorktreeValue() string {
	if m.isNewWorktree {
		return lipgloss.NewStyle().Foreground(successColor).Bold(true).Render("✓ yes — start on a fresh worktree")
	}
	return lipgloss.NewStyle().Foreground(dimColor).Render("no — use the current worktree")
}

// effectiveWorktreeBranch is the branch the session launches on. With the
// New-worktree toggle enabled and no explicit branch chosen, a fresh
// petname is minted so the host's WorktreeBranch resolve path creates an
// isolated worktree in the current project (off its default branch).
// TODO(ai-review): generate petname once on toggle-on and store in worktreeBranch to prevent orphaned worktrees on retry https://github.com/Acksell/clank/pull/73#discussion_r3449079137
func (m *SessionViewModel) effectiveWorktreeBranch() string {
	if m.isNewWorktree && m.worktreeBranch == "" {
		return petname.Generate()
	}
	return m.worktreeBranch
}

// openFolderPicker opens the folder picker browsing the current project's
// *parent*, so sibling projects are listed (switching project is the common
// case), with the current project highlighted among them.
func (m *SessionViewModel) openFolderPicker() tea.Cmd {
	m.showFolderPicker = true
	start := filepath.Dir(m.projectDir)
	if start == m.projectDir { // already at the filesystem root: no parent
		start = m.projectDir
	}
	m.folderPicker = newFolderPicker(start)
	if start != m.projectDir {
		// Land the cursor on the current project among its siblings.
		m.folderPicker.memory[start] = filepath.Base(m.projectDir)
		m.folderPicker.restoreCursor()
	}
	if m.height > 0 {
		m.folderPicker.maxRows = pickerRows(m.height)
	}
	return m.folderPicker.Init()
}

// applyProjectFolder repoints the compose session at dir and refreshes the
// per-repo agent/model catalog. Focus stays on returnTo (the row the picker
// was opened from) rather than jumping to the prompt.
func (m *SessionViewModel) applyProjectFolder(dir string, returnTo composeFocus) tea.Cmd {
	m.projectDir = dir
	m.gitRef = agent.GitRef{LocalPath: dir}
	if id, _ := agent.ReadLocalWorktreeID(dir); id != "" {
		m.gitRef.WorktreeID = id
	}
	m.composeFocus = returnTo

	cmds := []tea.Cmd{m.fetchModels()}
	if m.backend == agent.BackendOpenCode {
		cmds = append(cmds, m.fetchAgents())
	}
	return tea.Batch(cmds...)
}

// overlayFolderPicker composites the folder picker over the compose view.
func (m *SessionViewModel) overlayFolderPicker(base string) string {
	if !m.showFolderPicker {
		return base
	}
	return overlayCenter(base, m.folderPicker.View(), m.width, m.height)
}

// worktreeValue renders the current worktree (the project dir's basename).
func (m *SessionViewModel) worktreeValue() string {
	name := filepath.Base(m.projectDir)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = m.projectDir
	}
	return lipgloss.NewStyle().Foreground(textColor).Render(name)
}

// openWorktreePicker lists the current repo's worktrees and opens the picker.
// Worktrees are read locally (git worktree list); on a non-repo the picker
// surfaces that rather than failing.
func (m *SessionViewModel) openWorktreePicker() tea.Cmd {
	wts, err := listWorktrees(m.projectDir)
	m.worktreePicker = newWorktreePicker(wts, m.projectDir, pickerRows(m.height), err)
	m.showWorktreePicker = true
	return m.worktreePicker.Init()
}

// overlayWorktreePicker composites the worktree picker over the compose view.
func (m *SessionViewModel) overlayWorktreePicker(base string) string {
	if !m.showWorktreePicker {
		return base
	}
	return overlayCenter(base, m.worktreePicker.View(), m.width, m.height)
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

	// In composing mode always reserve the badge line, even when empty:
	// toggling backend changes whether a mode badge is present, and a
	// conditional line would resize the prompt box and jump the layout.
	if combinedBadge != "" || ctrlCHint != "" || m.composing {
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
