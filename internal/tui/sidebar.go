package tui

// SidebarModel is the navigation sidebar of the inbox layout.
//
// It contains two sections, selectable with one cursor:
//
//   - Worktrees (top): "All" plus one entry per unique GitRef.LocalPath
//     derived from cached sessions, sorted by most-recent UpdatedAt.
//   - Footer: "↓ Import Sessions" then "⚙ Settings", anchored to the
//     bottom of the sidebar.
//
// Cursor model: linear `cursor int` across all selectable rows. Layout:
//
//	[0]                 → "All" worktrees
//	[1 .. M]            → entries (M rows)
//	[M+1]               → "↓ Import Sessions" footer
//	[M+2]               → "⚙ Settings" footer
//
// Section boundaries are computed at use-time (cursorSection /
// settingsCursorIndex) so adding rows doesn't require renumbering.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
)

// sidebarWidth is the fallback width of the sidebar (including border)
// used when the screen width is not yet known.
const sidebarWidth = 30

// sidebarSection identifies which section the cursor is in. Used by
// key dispatch and by selection accessors that should return zero-value
// when the cursor isn't in their section.
type sidebarSection int

const (
	sectionWorktrees sidebarSection = iota
	sectionSettings
)

// worktreeEntry is one row in the worktrees section, derived from sessions.
type worktreeEntry struct {
	LocalPath       string
	Label           string // filepath.Base(LocalPath)
	Total           int
	Active          int
	Done            int
	Archived        int
	LatestUpdatedAt time.Time
}

// IsDone returns true when every session is done or archived.
func (e worktreeEntry) IsDone() bool {
	return e.Total > 0 && e.Active == 0
}

// IsArchived returns true when every session is archived.
func (e worktreeEntry) IsArchived() bool {
	return e.Total > 0 && e.Archived == e.Total
}

// branchWorktreeCreatedMsg is sent after a worktree is created for a new branch.
type branchWorktreeCreatedMsg struct {
	branch      string
	worktreeDir string
	err         error
}

// newWorktreeSessionRequestMsg is emitted after a worktree is created so the
// inbox can immediately open a composing session inside it.
type newWorktreeSessionRequestMsg struct {
	worktreeDir string
}

// SettingsRequestedMsg is emitted by the inbox when the user activates the
// "⚙ Settings" footer entry in the sidebar. It's defined here (rather than
// in inbox.go) so sidebar consumers can react without importing inbox types.
type SettingsRequestedMsg struct{}

// ImportSessionsRequestedMsg is emitted when the user activates the
// "↓ Import Sessions" footer entry in the sidebar.
type ImportSessionsRequestedMsg struct{}

// SidebarModel displays worktrees + a settings footer
type SidebarModel struct {
	client *daemonclient.Client
	// projectDir is the cwd the inbox was launched from. Kept for display
	// and for non-branch concerns (project filter); branch operations now
	// route through hostname/gitRef instead.
	projectDir string
	hostname   string
	gitRef     agent.GitRef

	entries []worktreeEntry
	cursor  int
	scroll  int

	// New branch input mode.
	creating bool
	input    textinput.Model

	// "All branches" is the virtual first entry (index -1 means all).
	// cursor==0 means "All branches", cursor>=1 means entries[cursor-1].
	focused bool
	width   int
	height  int
	err     error
}

// NewSidebarModel creates a sidebar for the given repo identity.
// projectDir is retained for display purposes only; branch/worktree ops
// are addressed by (hostname, gitRef).
func NewSidebarModel(client *daemonclient.Client, hostname string, gitRef agent.GitRef, projectDir string) SidebarModel {
	ti := textinput.New()
	ti.Placeholder = "branch-name"
	ti.CharLimit = 128
	ti.Prompt = "+ "
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(successColor).Bold(true)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)

	return SidebarModel{
		client:     client,
		hostname:   hostname,
		gitRef:     gitRef,
		projectDir: projectDir,
		input:      ti,
		cursor:     0, // "All" selected by default
	}
}

// Init is a no-op; the sidebar is populated via SetSessions.
func (m *SidebarModel) Init() tea.Cmd {
	return nil
}

// SetSessions rebuilds the worktree entries from the provided sessions.
// Entries are derived by grouping on GitRef.LocalPath and sorted by most-recent UpdatedAt.
func (m *SidebarModel) SetSessions(sessions []agent.SessionInfo) {
	type entryAcc struct {
		worktreeEntry
	}
	byPath := make(map[string]*entryAcc)
	for _, s := range sessions {
		path := s.GitRef.LocalPath
		if path == "" {
			continue
		}
		acc := byPath[path]
		if acc == nil {
			acc = &entryAcc{worktreeEntry{
				LocalPath: path,
				Label:     filepath.Base(path),
			}}
			byPath[path] = acc
		}
		acc.Total++
		switch s.Visibility {
		case agent.VisibilityArchived:
			acc.Archived++
		case agent.VisibilityDone:
			acc.Done++
		default:
			acc.Active++
		}
		if s.UpdatedAt.After(acc.LatestUpdatedAt) {
			acc.LatestUpdatedAt = s.UpdatedAt
		}
	}

	entries := make([]worktreeEntry, 0, len(byPath))
	for _, acc := range byPath {
		entries = append(entries, acc.worktreeEntry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].LatestUpdatedAt.After(entries[j].LatestUpdatedAt)
	})

	m.entries = entries

	// Clamp cursor so it stays valid after the list shrinks.
	if max := m.settingsCursorIndex(); m.cursor > max {
		m.cursor = max
	}
	// Clamp scroll: renderWorktreeEntries slices m.entries[m.scroll:end],
	// which would panic if the list shrinks below the previous offset.
	switch {
	case len(m.entries) == 0:
		m.scroll = 0
	case m.scroll > len(m.entries)-1:
		m.scroll = len(m.entries) - 1
	}
}

// --- Cursor / section helpers ---

// totalRows is the number of selectable rows across all sections.
// Layout: [1 "All"][len(entries) entries][1 import][1 settings].
func (m *SidebarModel) totalRows() int {
	return 1 + len(m.entries) + 2
}

// importCursorIndex returns the cursor value of the "↓ Import Sessions"
// footer row. Always second-to-last.
func (m *SidebarModel) importCursorIndex() int {
	return m.totalRows() - 2
}

// settingsCursorIndex returns the cursor value of the "⚙ Settings"
// footer row. Always the last row in the sidebar.
func (m *SidebarModel) settingsCursorIndex() int {
	return m.totalRows() - 1
}

// CursorOnImport reports whether the cursor is on the import row.
func (m *SidebarModel) CursorOnImport() bool {
	return m.cursor == m.importCursorIndex()
}

// CursorOnSettings reports whether the cursor is on the settings row.
func (m *SidebarModel) CursorOnSettings() bool {
	return m.cursor == m.settingsCursorIndex()
}

// cursorSection returns which section the cursor is in and the
// section-local index. For sectionWorktrees, idx==0 means the "All"
// row; idx>=1 means entries[idx-1]. For sectionSettings, idx is
// always 0 (single row).
func (m *SidebarModel) cursorSection() (sidebarSection, int) {
	if m.cursor >= m.importCursorIndex() {
		return sectionSettings, 0
	}
	return sectionWorktrees, m.cursor
}

// sectionBreakpoints returns the cursor positions that shift+up/shift+down
// snap between. For the entries section both the first (1) and last entry are
// included so shift+up stops at the top of the list before jumping to "All".
func (m *SidebarModel) sectionBreakpoints() []int {
	bp := []int{0}
	if last := len(m.entries); last > 0 {
		bp = append(bp, 1) // first entry — top of list
		if last > 1 {
			bp = append(bp, last) // last entry — bottom of list
		}
	}
	bp = append(bp, m.importCursorIndex())
	bp = append(bp, m.settingsCursorIndex())
	return bp
}

// SelectedBranch returns the LocalPath for the currently selected entry.
// Empty string means "All" or the settings row is selected.
// This is kept for call-site compatibility; prefer SelectedWorktreeDir
// for semantic clarity.
func (m *SidebarModel) SelectedBranch() string {
	return m.SelectedWorktreeDir()
}

// SelectedWorktreeDir returns the worktree directory path for the currently
// selected entry. Empty string means "all worktrees" (no filter).
func (m *SidebarModel) SelectedWorktreeDir() string {
	if m.cursor == 0 || len(m.entries) == 0 {
		return ""
	}
	idx := m.cursor - 1
	if idx >= len(m.entries) {
		return ""
	}
	return m.entries[idx].LocalPath
}

// SelectedBranchInfo always returns nil.
// TODO: merge overlay disabled until sessions carry git branch metadata.
func (m *SidebarModel) SelectedBranchInfo() *host.BranchInfo {
	return nil
}

// SetFocused sets whether the sidebar has keyboard focus.
func (m *SidebarModel) SetFocused(focused bool) {
	m.focused = focused
}

// Focused returns whether the sidebar has keyboard focus.
func (m *SidebarModel) Focused() bool {
	return m.focused
}

// SetSize sets the sidebar dimensions.
func (m *SidebarModel) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// Update handles messages for the sidebar.
func (m *SidebarModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case branchWorktreeCreatedMsg:
		if msg.err != nil {
			m.err = msg.err
			return nil
		}
		// Clear any prior ResolveWorktree error so a successful retry
		// doesn't render a stale message while opening the new session.
		m.err = nil
		m.creating = false
		m.input.SetValue("")
		// Emit a request to open a composing session in the new worktree.
		return func() tea.Msg {
			return newWorktreeSessionRequestMsg{worktreeDir: msg.worktreeDir}
		}
	}

	if m.creating {
		return m.updateCreating(msg)
	}

	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		return m.handleKey(keyMsg)
	}

	return nil
}

// handleKey handles keyboard input when focused and not creating.
func (m *SidebarModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	msg = normalizeKeyCase(msg)

	maxIdx := m.settingsCursorIndex() // last row (settings footer)

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
		if m.cursor > 0 {
			m.cursor--
		} else {
			m.cursor = maxIdx
		}
		m.ensureVisible()
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
		if m.cursor < maxIdx {
			m.cursor++
		} else {
			m.cursor = 0
		}
		m.ensureVisible()
	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+up"))):
		m.cursor = prevBreakpoint(m.sectionBreakpoints(), m.cursor)
		m.ensureVisible()
	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+down"))):
		m.cursor = nextBreakpoint(m.sectionBreakpoints(), m.cursor)
		m.ensureVisible()
	case key.Matches(msg, key.NewBinding(key.WithKeys("home", "g"))):
		m.cursor = 0
		m.ensureVisible()
	case key.Matches(msg, key.NewBinding(key.WithKeys("end", "G"))):
		m.cursor = maxIdx
		m.ensureVisible()
	case key.Matches(msg, key.NewBinding(key.WithKeys("n"))):
		// New worktree only makes sense in the worktrees section; pressing
		// 'n' on the Settings row should be a no-op, not open the prompt.
		if sec, _ := m.cursorSection(); sec == sectionWorktrees {
			m.creating = true
			m.input.SetValue("")
			return m.input.Focus()
		}
	}

	return nil
}

// updateCreating handles input while creating a new branch.
func (m *SidebarModel) updateCreating(msg tea.Msg) tea.Cmd {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		keyMsg = normalizeKeyCase(keyMsg)

		switch {
		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("esc"))):
			m.creating = false
			m.input.SetValue("")
			return nil

		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("enter"))):
			name := strings.TrimSpace(m.input.Value())
			if name == "" {
				m.creating = false
				return nil
			}
			return m.createWorktree(name)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// View renders the sidebar.
func (m *SidebarModel) View() string {
	w := m.width
	if w <= 0 {
		w = sidebarWidth
	}

	// Inner content width available after the border. lipgloss v2 treats
	// the style's Width as the total rendered width (border included), so
	// we subtract 2 here and pass the full `w` as the outer Width below.
	// A further -2 buffer is kept so lines never render exactly up to the
	// right edge — that margin is what prevents wrap from tiny rounding or
	// emoji-width mismatches (the ⚙ in the footer can be double-width in
	// some fonts).
	contentWidth := w - 4
	if contentWidth < 10 {
		contentWidth = 10
	}

	var lines []string

	// Header.
	header := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render("Worktrees")
	lines = append(lines, header)
	lines = append(lines, "")

	// "All" entry (no filter).
	allLabel := "  All"
	if m.cursor == 0 && m.focused {
		allLabel = lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ") +
			lipgloss.NewStyle().Foreground(textColor).Bold(true).Render("All")
	} else if m.cursor == 0 {
		allLabel = lipgloss.NewStyle().Foreground(textColor).Render("  All")
	} else {
		allLabel = lipgloss.NewStyle().Foreground(dimColor).Render("  All")
	}
	lines = append(lines, allLabel)
	lines = append(lines, "")

	// When creating a new worktree with cursor on "All", show input here.
	if m.creating && m.cursor == 0 {
		m.input.SetWidth(contentWidth - 2)
		lines = append(lines, "  "+m.input.View())
		lines = append(lines, "")
	}

	// Worktree entries derived from sessions (scrolled; input inserted at cursor).
	lines = append(lines, m.renderWorktreeEntries(contentWidth)...)

	// Error.
	if m.err != nil {
		lines = append(lines, "")
		errLine := lipgloss.NewStyle().Foreground(dangerColor).
			Render(truncateStr(m.err.Error(), contentWidth))
		lines = append(lines, errLine)
	}

	// Footer: pad with blank lines to push the footer rows to the bottom
	// of the sidebar, separated from the entry list by a dim rule.
	listH := m.listHeight()
	footer := m.renderFooter(contentWidth)
	footerLines := strings.Count(footer, "\n") + 1
	if pad := listH - len(lines) - footerLines; pad > 0 {
		for i := 0; i < pad; i++ {
			lines = append(lines, "")
		}
	}
	lines = append(lines, footer)

	content := strings.Join(lines, "\n")

	// Wrap in a focus-aware pane border (shared with the right pane so
	// both panes use one source of truth for focus-vs-unfocused styling).
	style := paneBorderStyle(m.focused).
		Width(w - paneBorderInset).
		Height(m.listHeight())

	return style.Render(content)
}

// renderWorktreeEntries renders the visible slice of worktree entries, applying
// the scroll offset. When in creating mode, the branch-name input is inserted
// inline after the cursor's entry (or after "All" if cursor==0, handled by View).
func (m *SidebarModel) renderWorktreeEntries(contentWidth int) []string {
	if len(m.entries) == 0 {
		return nil
	}
	vh := m.entryViewportH()
	end := m.scroll + vh
	if end > len(m.entries) {
		end = len(m.entries)
	}
	visible := m.entries[m.scroll:end]

	lines := make([]string, 0, len(visible)+2)
	for i, e := range visible {
		idx := m.scroll + i + 1 // cursor index (0 = All)
		lines = append(lines, m.renderWorktreeEntry(e, idx, contentWidth))
		if m.creating && m.cursor == idx {
			m.input.SetWidth(contentWidth - 2)
			lines = append(lines, "  "+m.input.View())
		}
	}
	return lines
}

// renderWorktreeEntry renders a single worktree entry with session count badge.
func (m *SidebarModel) renderWorktreeEntry(e worktreeEntry, idx, maxWidth int) string {
	selected := m.cursor == idx && m.focused

	countBadge := ""
	badgeColor := dimColor
	if e.Total > 0 {
		if e.IsArchived() {
			countBadge = fmt.Sprintf(" (%d)", e.Total)
			badgeColor = mutedColor
		} else if e.IsDone() {
			countBadge = fmt.Sprintf(" (%d)", e.Total)
			badgeColor = successColor
		} else {
			countBadge = fmt.Sprintf(" (%d)", e.Active)
		}
	}

	// Truncate label to fit.
	label := e.Label
	maxLabel := maxWidth - 2 - len(countBadge) // 2 for prefix
	if maxLabel < 6 {
		maxLabel = 6
	}
	if len(label) > maxLabel {
		label = label[:maxLabel-1] + "…"
	}

	nameStyle := lipgloss.NewStyle()
	if selected {
		nameStyle = nameStyle.Foreground(textColor).Bold(true)
	} else if e.IsArchived() {
		nameStyle = nameStyle.Foreground(mutedColor)
	} else if e.IsDone() {
		nameStyle = nameStyle.Foreground(dimColor)
	} else {
		nameStyle = nameStyle.Foreground(textColor)
	}

	prefix := "  "
	if selected {
		prefix = lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
	}

	line := prefix + nameStyle.Render(label)
	line += lipgloss.NewStyle().Foreground(badgeColor).Render(countBadge)
	return line
}

// renderFooter renders the bottom-anchored block of the sidebar containing
// "↓ Import Sessions" and "⚙ Settings".
func (m *SidebarModel) renderFooter(maxWidth int) string {
	sep := lipgloss.NewStyle().
		Foreground(mutedColor).
		Render(strings.Repeat("─", maxWidth))

	importLabel := "↓ Import Sessions"
	importSelected := m.CursorOnImport() && m.focused
	var importRow string
	if importSelected {
		prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		name := lipgloss.NewStyle().Foreground(textColor).Bold(true).Render(importLabel)
		importRow = prefix + name
	} else if m.CursorOnImport() {
		importRow = lipgloss.NewStyle().Foreground(textColor).Render("  " + importLabel)
	} else {
		importRow = lipgloss.NewStyle().Foreground(dimColor).Render("  " + importLabel)
	}

	settingsLabel := "⚙ Settings"
	settingsSelected := m.CursorOnSettings() && m.focused
	var settingsRow string
	if settingsSelected {
		prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		name := lipgloss.NewStyle().Foreground(textColor).Bold(true).Render(settingsLabel)
		settingsRow = prefix + name
	} else if m.CursorOnSettings() {
		settingsRow = lipgloss.NewStyle().Foreground(textColor).Render("  " + settingsLabel)
	} else {
		settingsRow = lipgloss.NewStyle().Foreground(dimColor).Render("  " + settingsLabel)
	}

	return sep + "\n" + importRow + "\n" + settingsRow
}

// listHeight returns the height available for the body (excluding border).
func (m *SidebarModel) listHeight() int {
	h := m.height - 4 // border top/bottom + some padding
	if h < 5 {
		h = 5
	}
	return h
}

// entryViewportH returns the number of entry rows that fit in the scrollable
// middle section. Fixed lines consumed: header(1)+blank(1)+All(1)+blank(1)+
// footer_sep(1)+import(1)+settings(1) = 7. When creating, the inline input
// takes 2 additional lines.
func (m *SidebarModel) entryViewportH() int {
	extra := 0
	if m.creating {
		extra = 2
	}
	vh := m.listHeight() - 7 - extra
	if vh < 1 {
		vh = 1
	}
	return vh
}

// ensureVisible scrolls the entries section to keep the cursor visible.
// "All" and footer rows are always visible; only the entries section scrolls.
func (m *SidebarModel) ensureVisible() {
	if m.cursor == 0 || m.cursor >= m.importCursorIndex() {
		// "All" and footer rows are always visible; no scroll adjustment needed.
		return
	}
	entryIdx := m.cursor - 1
	vh := m.entryViewportH()
	if entryIdx < m.scroll {
		m.scroll = entryIdx
	}
	if entryIdx >= m.scroll+vh {
		m.scroll = entryIdx - vh + 1
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

// createWorktree asks the daemon to create a worktree for the given branch.
func (m *SidebarModel) createWorktree(branch string) tea.Cmd {
	client := m.client
	hostname := m.hostname
	gitRef := m.gitRef
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		wt, err := client.Host(hostname).ResolveWorktree(ctx, gitRef, branch)
		if err != nil {
			return branchWorktreeCreatedMsg{branch: branch, err: err}
		}
		return branchWorktreeCreatedMsg{branch: branch, worktreeDir: wt.WorktreeDir}
	}
}
