package tui

// Composing mode for SessionViewModel: the user types their first prompt
// before any daemon session exists. On send, the session is created and
// the view transitions to the normal streaming session view.

import (
	"context"
	"fmt"
	"image/color"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/agent/presets"
	"github.com/supaclank/clank/internal/config"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/host/petname"
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
// The gitRef is built from projectDir (LocalPath) plus the stamped
// worktree ID, if any (read via agent.ReadLocalWorktreeID), so the
// on-demand config-options probe can target it. Without a stamp the ref
// is local-only and any cross-host operations will fail at launch.
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
	// stamped worktree ID (cross-host stable identity), when present.
	// See agent.GitRef godoc.
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
	// Presets are host-served and fetched (Init → fetchPresets); nothing
	// is seeded here, so compose never offers a config nobody declared.
	return m
}

// composeConfig assembles the create-time config: the selected preset's
// bundle (the Default preset carries every required key) overlaid with
// the model picker's explicit override. Nil until presets load — the
// host then rejects the create with the missing keys named, which is
// the fail-fast contract.
func (m *SessionViewModel) composeConfig() map[string]string {
	sel := m.composeSelectedPreset()
	if sel == nil {
		return nil
	}
	cfg := make(map[string]string, len(sel.Config)+1)
	for k, v := range sel.Config {
		cfg[k] = v
	}
	if m.selectedModel >= 0 && m.selectedModel < len(m.models) {
		cfg[agent.ConfigOptionModel] = m.models[m.selectedModel].ID
	}
	return cfg
}

// composeSelectedPreset returns the Tab-selected preset for the compose
// backend, nil until the host's preset list has loaded. m.presets is
// already backend-scoped (fetchPresets filters server-side and
// presetsResultMsg is gated on the backend still matching).
func (m *SessionViewModel) composeSelectedPreset() *presets.Preset {
	if len(m.presets) == 0 {
		return nil
	}
	if m.selectedPreset < 0 || m.selectedPreset >= len(m.presets) {
		return &m.presets[0]
	}
	return &m.presets[m.selectedPreset]
}

// handleComposeConfigOptions lands the on-demand /config-options probe
// the compose model picker is waiting on: build the picker from the
// model option's values, or surface why there is nothing to pick.
func (m *SessionViewModel) handleComposeConfigOptions(msg configOptionsResultMsg) (tea.Model, tea.Cmd) {
	m.modelOptionsLoading = false
	if msg.backend != m.backend {
		// Stale probe from before a backend toggle; drop it.
		return m, nil
	}
	if msg.err != nil {
		m.showModelPicker = false
		m.err = fmt.Errorf("load model options: %w", msg.err)
		return m, m.input.Focus()
	}
	models := modelInfosFromConfigOptions(msg.options)
	if len(models) == 0 {
		m.showModelPicker = false
		m.err = fmt.Errorf("%s advertises no model choice here", m.backend)
		return m, m.input.Focus()
	}
	m.models = models

	// Preselect: the user's earlier pick this compose, else the saved
	// per-backend preference, else the agent's own current value —
	// highlight only, nothing is sent unless the user confirms.
	sel := m.selectedModel
	if sel < 0 {
		prefs, _ := config.LoadPreferences()
		if pref := prefs.ModelFor(string(m.backend)); !pref.IsZero() {
			sel = slices.IndexFunc(models, func(mi agent.ModelInfo) bool {
				return mi.ID == pref.ModelID && mi.ProviderID == pref.ProviderID
			})
		}
	}
	if sel < 0 {
		if mo := modelOptionFromConfig(msg.options); mo != nil {
			sel = slices.IndexFunc(models, func(mi agent.ModelInfo) bool {
				return mi.ID == mo.CurrentValue
			})
		}
	}
	m.modelPicker = newModelPicker(models, sel, m.backend)
	return m, m.modelPicker.Init()
}

// updateCompose handles all messages while in composing mode.
func (m *SessionViewModel) updateCompose(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Model picker takes priority when open.
	if m.showModelPicker {
		switch msg := msg.(type) {
		case configOptionsResultMsg:
			return m.handleComposeConfigOptions(msg)
		case modelPickerResultMsg:
			m.showModelPicker = false
			m.selectedModel = msg.selectedModel
			backend, pref := m.snapshotModelPreference()
			go persistModelPreference(backend, pref)
			return m, m.input.Focus()
		case modelPickerCancelMsg:
			m.showModelPicker = false
			m.modelOptionsLoading = false
			return m, m.input.Focus()
		case modelPickerConnectProviderMsg:
			m.showModelPicker = false
			backend := m.backend
			return m, func() tea.Msg { return openProviderAuthFromSessionMsg{backend: backend} }
		case tea.KeyPressMsg:
			// While the probe is in flight only esc (cancel) applies; the
			// picker component doesn't exist yet to take other keys.
			if m.modelOptionsLoading {
				if key.Matches(msg, key.NewBinding(key.WithKeys("esc"))) {
					m.showModelPicker = false
					m.modelOptionsLoading = false
					return m, m.input.Focus()
				}
				return m, nil
			}
			var cmd tea.Cmd
			m.modelPicker, cmd = m.modelPicker.Update(msg)
			return m, cmd
		default:
			if m.modelOptionsLoading {
				return m, nil
			}
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

	case presetsResultMsg:
		if msg.backend == m.backend {
			// Keep the user's pick by id across a refetch (backend
			// toggled away and back) when the list still offers it.
			prevID := ""
			if p := m.composeSelectedPreset(); p != nil {
				prevID = p.ID
			}
			m.presets = msg.presets
			m.selectedPreset = 0
			for i, p := range m.presets {
				if p.ID == prevID {
					m.selectedPreset = i
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

	case tea.PasteMsg:
		// A dropped/pasted image-file path becomes a pill; anything else
		// falls through to the textarea as normal text.
		if m.composeFocus == focusPrompt && m.maybePasteImagePath(msg.Content) {
			return m, nil
		}

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
		// fires when no picker is open.) Compose is an overlay over
		// whatever the right pane was previously showing — Esc restores
		// that state, it doesn't route to the inbox screen.
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
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+v"))):
		// Paste an image from the clipboard as an inline pill. No-op when the
		// clipboard holds no image (plain text paste arrives as tea.PasteMsg).
		m.pasteClipboardImage()
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
		// A pill deletes as one unit when the cursor is right after it;
		// otherwise fall through to the textarea's normal backspace.
		if m.deletePillBeforeCursor(msg) {
			return m, nil
		}

	case key.Matches(msg, key.NewBinding(key.WithKeys("up"))):
		// Escape upward to the row above only when the cursor is already
		// on the first line; otherwise let the textarea move the cursor.
		if m.input.Line() == 0 {
			return m, m.moveComposeFocus(-1)
		}

	case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
		// Cycle agent presets (Default, Plan, user-saved) for the backend.
		if len(m.presets) > 1 {
			m.selectedPreset = (m.selectedPreset + 1) % len(m.presets)
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))):
		// Open the model picker: probe the agent's live config options
		// first (the host opens a short-lived session), showing the
		// loading overlay until they land.
		m.showModelPicker = true
		m.modelOptionsLoading = true
		return m, m.fetchConfigOptions()

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

	prompt, atts := m.promptForSend()
	if prompt == "" && len(atts) == 0 {
		m.err = fmt.Errorf("prompt is required")
		return m, nil
	}
	if m.projectDir == "" {
		m.err = fmt.Errorf("project directory is required")
		return m, nil
	}
	cfg := m.composeConfig()
	if cfg == nil {
		// Presets for this backend haven't loaded yet — the host would
		// reject this with 400 config_incomplete anyway; gate here so
		// the user gets an immediate reason instead of a round-trip error.
		m.err = fmt.Errorf("presets still loading — try again in a moment")
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
		Backend:     m.backend,
		Hostname:    host.HostLocal,
		GitRef:      gitRef,
		Prompt:      prompt,
		Attachments: atts,
	}
	// cfg already carries the model override (composeConfig) — the
	// config channel is the only model channel in compose, using the
	// agent's own advertised value ids.
	req.Config = cfg

	return m, m.createSessionCmd(req)
}

// createSessionCmd subscribes to SSE first, then creates the session. This
// avoids the race where events are emitted before the TUI subscribes. Image
// attachments ride in req as file:///data: sources clank-host resolves itself.
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

	// Show the user's prompt as the first entry, tagged with the preset
	// it launched under.
	modeLabel := ""
	if p := m.composeSelectedPreset(); p != nil {
		modeLabel = p.Name
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

// presetColor is the badge color for a preset: cyan for the read-only
// Plan built-in (mirroring the plan permission-mode color), green
// otherwise.
func presetColor(p presets.Preset) color.Color {
	if strings.HasPrefix(p.ID, presets.BuiltinPlanPrefix) {
		return secondaryColor
	}
	return successColor
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
		helpParts = []string{"↑: fields", "enter: launch", "shift+enter: newline", "ctrl+v: image", "ctrl+b: toggle backend"}
		if len(m.presets) > 1 {
			helpParts = append(helpParts, "tab: cycle preset")
		}
		helpParts = append(helpParts, "shift+tab: model", qLabel)
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
	parts := make([]string, len(agent.AllBackends))
	for i, b := range agent.AllBackends {
		parts[i] = option(backendDisplayName(b), i, m.backend == b)
	}
	return strings.Join(parts, "  ")
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
// TODO(ai-review): generate petname once on toggle-on and store in worktreeBranch to prevent orphaned worktrees on retry https://github.com/supaclank/clank/pull/73#discussion_r3449079137
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

// applyProjectFolder repoints the compose session at dir. Focus stays on
// returnTo (the row the picker was opened from) rather than jumping to
// the prompt.
func (m *SessionViewModel) applyProjectFolder(dir string, returnTo composeFocus) tea.Cmd {
	m.projectDir = dir
	m.gitRef = agent.GitRef{LocalPath: dir}
	if id, _ := agent.ReadLocalWorktreeID(dir); id != "" {
		m.gitRef.WorktreeID = id
	}
	m.composeFocus = returnTo

	// Model options are project-scoped (opencode aggregates per-repo
	// providers), so a pick made under the previous folder must not ride
	// into the new one as a stale config override. Presets are
	// host-scoped and stay. The picker re-probes on next open.
	m.models, m.selectedModel = nil, -1
	return nil
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

// modelBadgeLabel formats a model for the compose badge. Some agents
// (e.g. opencode) already advertise provider-qualified value ids, so the
// group prefix is added only when it isn't already there — otherwise the
// badge would double up ("opencode/opencode/big-pickle").
func modelBadgeLabel(model agent.ModelInfo) string {
	if model.ProviderID != "" && !strings.HasPrefix(model.ID, model.ProviderID+"/") {
		return model.ProviderID + "/" + model.ID
	}
	return model.ID
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

	if m.composing {
		if p := m.composeSelectedPreset(); p != nil {
			mc := presetColor(*p)
			modeBadge = lipgloss.NewStyle().Foreground(mc).Bold(true).Render(p.Name)
			if m.input.Focused() {
				bc = mc
			}
		} else if m.input.Focused() {
			bc = primaryColor
		}
	} else if len(m.modes) > 0 {
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
		label := modelBadgeLabel(m.models[m.selectedModel])
		modelBadge = lipgloss.NewStyle().Foreground(secondaryColor).Render(label)
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
	inner.WriteString(m.highlightPills(m.input.View()))

	return promptInputStyleWithColor(bc).Render(inner.String())
}
