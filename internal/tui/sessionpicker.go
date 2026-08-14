package tui

// Session picker — `clank preview --attach` as a standalone bubbletea
// program: choose an existing agent session for the preview overlay to
// bind to, newest activity first, with an in-list "Rediscover" action
// for sessions clank hasn't registered yet.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/supaclank/clank/internal/agent"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
)

// SessionPickerResult is what the CLI reads off the model after the
// program exits. No SessionID means the user canceled (esc/ctrl+c) and
// the preview run should stop — --attach was explicit, so there is no
// silent fall-through to a fresh session.
type SessionPickerResult struct {
	SessionID string
	IsAborted bool
}

type sessionPickerPhase int

const (
	sessionPickerLoading sessionPickerPhase = iota
	sessionPickerList
	sessionPickerDiscovering
	sessionPickerError
)

// sessionPickerIndexRediscover is the synthetic action row that re-runs
// backend session discovery — the "I can't see my session" escape
// hatch. Anchored as the first row so it's visible without scrolling,
// though the cursor starts on the newest session below it.
const sessionPickerIndexRediscover = -1

// sessionPickerListTimeout caps the initial catalog read; the local
// daemon answers in milliseconds.
const sessionPickerListTimeout = 30 * time.Second

// sessionPickerDiscoverTimeout matches the inbox's user-initiated import
// budget — backends scan on-disk session archives, which can be slow.
const sessionPickerDiscoverTimeout = 60 * time.Second

// sessionPickerLoadedMsg carries the session catalog (or its failure).
type sessionPickerLoadedMsg struct {
	sessions []agent.SessionInfo
	err      error
}

// sessionPickerDiscoveredMsg carries the outcome of a rediscover run.
type sessionPickerDiscoveredMsg struct {
	imported int
	err      error
}

// sessionPickerItem is one list entry: index >= 0 into the sessions
// slice, or sessionPickerIndexRediscover for the action row.
type sessionPickerItem struct {
	index   int
	display string // lowercase haystack for filtering
}

// SessionPickerModel drives the attach-session picker end to end.
type SessionPickerModel struct {
	client     *daemonclient.Client
	projectDir string

	phase    sessionPickerPhase
	errMsg   string
	notice   string // outcome line after a rediscover run
	sessions []agent.SessionInfo
	items    []sessionPickerItem
	filtered []sessionPickerItem

	cursor    int
	scroll    int
	maxRows   int
	search    textinput.Model
	lastQuery string

	result   SessionPickerResult
	quitting bool // render nothing after quit so the list leaves no scrollback
	spinner  spinner.Model
}

// NewSessionPickerModel returns the picker program. projectDir seeds
// the rediscover action's backend scan.
func NewSessionPickerModel(client *daemonclient.Client, projectDir string) *SessionPickerModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(primaryColor)

	ti := textinput.New()
	ti.Placeholder = "Search..."
	ti.CharLimit = 128
	ti.Prompt = "/ "
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(dimColor)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)
	ti.SetWidth(sessionPickerInnerWidth)
	ti.Focus()

	return &SessionPickerModel{
		client:     client,
		projectDir: projectDir,
		phase:      sessionPickerLoading,
		maxRows:    12,
		search:     ti,
		spinner:    sp,
	}
}

// Result reports what the run chose. Read after the program exits.
func (m *SessionPickerModel) Result() SessionPickerResult {
	return m.result
}

func (m *SessionPickerModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.loadCmd())
}

// loadCmd fetches the session catalog from the daemon.
func (m *SessionPickerModel) loadCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), sessionPickerListTimeout)
		defer cancel()
		sessions, err := client.Sessions().List(ctx)
		return sessionPickerLoadedMsg{sessions: sessions, err: err}
	}
}

// discoverCmd re-runs backend session discovery for the project dir and
// reports how many sessions it newly registered (the same sweep the
// inbox's import action runs).
func (m *SessionPickerModel) discoverCmd() tea.Cmd {
	client := m.client
	projectDir := m.projectDir
	before := len(m.sessions)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), sessionPickerDiscoverTimeout)
		defer cancel()
		backends := []agent.BackendType{agent.BackendOpenCode, agent.BackendClaudeCode}
		var errs []string
		for _, bt := range backends {
			if err := client.Sessions().Discover(ctx, bt, projectDir); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", bt, err))
			}
		}
		if len(errs) == len(backends) {
			return sessionPickerDiscoveredMsg{err: fmt.Errorf("discover sessions: %s", strings.Join(errs, "; "))}
		}
		after, err := client.Sessions().List(ctx)
		if err != nil {
			return sessionPickerDiscoveredMsg{err: err}
		}
		imported := len(visibleSessionsByActivity(after)) - before
		if imported < 0 {
			imported = 0
		}
		return sessionPickerDiscoveredMsg{imported: imported}
	}
}

func (m *SessionPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case sessionPickerLoadedMsg:
		if msg.err != nil {
			m.phase = sessionPickerError
			m.errMsg = msg.err.Error()
			return m, nil
		}
		m.setSessions(visibleSessionsByActivity(msg.sessions))
		m.phase = sessionPickerList
		return m, nil

	case sessionPickerDiscoveredMsg:
		if msg.err != nil {
			m.phase = sessionPickerList
			m.notice = "rediscover failed: " + msg.err.Error()
			return m, nil
		}
		if msg.imported == 1 {
			m.notice = "imported 1 new session"
		} else {
			m.notice = fmt.Sprintf("imported %d new sessions", msg.imported)
		}
		return m, m.loadCmd()

	case tea.KeyPressMsg:
		return m.handleKey(normalizeKeyCase(msg))

	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			if m.cursor > 0 {
				m.cursor--
				m.ensureVisible()
			}
		case tea.MouseWheelDown:
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
				m.ensureVisible()
			}
		}
		return m, nil

	// Swallow other captured mouse events so clicks don't leak into the
	// search input as stray updates.
	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m.updateSearch(msg)
}

func (m *SessionPickerModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		return m.abort()

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		// A first esc backs out of a typed filter; only a bare esc cancels.
		if m.search.Value() != "" {
			m.search.SetValue("")
			m.lastQuery = ""
			m.applyFilter()
			return m, nil
		}
		return m.abort()

	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		if m.phase != sessionPickerList || m.cursor < 0 || m.cursor >= len(m.filtered) {
			return m, nil
		}
		idx := m.filtered[m.cursor].index
		if idx == sessionPickerIndexRediscover {
			m.phase = sessionPickerDiscovering
			m.notice = ""
			return m, m.discoverCmd()
		}
		m.result = SessionPickerResult{SessionID: m.sessions[idx].ID}
		m.quitting = true
		return m, tea.Quit

	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
		if m.cursor > 0 {
			m.cursor--
			m.ensureVisible()
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
			m.ensureVisible()
		}
		return m, nil
	}

	return m.updateSearch(msg)
}

// abort ends the run without a session — the caller stops the preview.
func (m *SessionPickerModel) abort() (tea.Model, tea.Cmd) {
	m.result = SessionPickerResult{IsAborted: true}
	m.quitting = true
	return m, tea.Quit
}

// updateSearch forwards msg to the text input and re-filters on change.
func (m *SessionPickerModel) updateSearch(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	if q := m.search.Value(); q != m.lastQuery {
		m.lastQuery = q
		m.applyFilter()
	}
	return m, cmd
}

// setSessions installs a freshly-loaded catalog and rebuilds the rows,
// preserving the current filter query. The cursor lands on the newest
// session (the rediscover action stays one step up).
func (m *SessionPickerModel) setSessions(sessions []agent.SessionInfo) {
	m.sessions = sessions
	m.items = make([]sessionPickerItem, 0, len(sessions)+1)
	m.items = append(m.items, sessionPickerItem{index: sessionPickerIndexRediscover})
	for i, s := range sessions {
		m.items = append(m.items, sessionPickerItem{
			index:   i,
			display: strings.ToLower(sessionPickerHaystack(s)),
		})
	}
	m.cursor = 0
	if len(sessions) > 0 {
		m.cursor = 1
	}
	m.applyFilter()
}

// visibleSessionsByActivity drops done/archived sessions and sorts the
// rest by last activity, newest first.
func visibleSessionsByActivity(sessions []agent.SessionInfo) []agent.SessionInfo {
	out := make([]agent.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		if s.Hidden() {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// sessionPickerHaystack is the text a filter query matches against.
func sessionPickerHaystack(s agent.SessionInfo) string {
	return strings.Join([]string{
		sessionPickerTitle(s),
		string(s.Backend),
		filepath.Base(s.GitRef.LocalPath),
		s.ID,
		s.ExternalID,
	}, "  ")
}

// sessionPickerTitle picks the row's primary label: the AI title, else
// the first line of the opening prompt, else a placeholder.
func sessionPickerTitle(s agent.SessionInfo) string {
	if t := strings.TrimSpace(s.Title); t != "" {
		return t
	}
	prompt := strings.TrimSpace(s.Prompt)
	if line, _, _ := strings.Cut(prompt, "\n"); strings.TrimSpace(line) != "" {
		return strings.TrimSpace(line)
	}
	return "(untitled)"
}

// applyFilter rebuilds the filtered rows from the query. The rediscover
// action stays anchored first regardless of the query — its whole
// reason to exist is that the session being searched for isn't in the
// list.
func (m *SessionPickerModel) applyFilter() {
	q := strings.ToLower(m.search.Value())
	if q == "" {
		m.filtered = m.items
	} else {
		m.filtered = []sessionPickerItem{{index: sessionPickerIndexRediscover}}
		for _, item := range m.items {
			if item.index == sessionPickerIndexRediscover {
				continue // already anchored first
			}
			if strings.Contains(item.display, q) {
				m.filtered = append(m.filtered, item)
			}
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.scroll = 0
	m.ensureVisible()
}

// ensureVisible adjusts scroll so the cursor stays within the window.
func (m *SessionPickerModel) ensureVisible() {
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+m.maxRows {
		m.scroll = m.cursor - m.maxRows + 1
	}
}

const (
	sessionPickerMenuWidth  = 72
	sessionPickerInnerWidth = sessionPickerMenuWidth - 4 // border + padding
)

func (m *SessionPickerModel) View() tea.View {
	if m.quitting {
		// Render nothing so the inline renderer erases the list on exit
		// instead of leaving it in terminal history.
		return connectView("")
	}
	v := connectView(m.body())
	// Mouse capture works inline too (it's a terminal mode, not an
	// alt-screen feature): wheel scrolls the cursor for the picker's
	// lifetime, and the terminal gets its own scroll back on exit.
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m *SessionPickerModel) body() string {
	var sb strings.Builder

	sb.WriteString(lipgloss.NewStyle().
		Bold(true).
		Foreground(textColor).
		Width(sessionPickerInnerWidth).
		Render("Attach to a session"))
	sb.WriteString("\n")

	switch m.phase {
	case sessionPickerLoading:
		sb.WriteString(m.spinner.View() + " Loading sessions…\n")
	case sessionPickerDiscovering:
		sb.WriteString(m.spinner.View() + " Rediscovering sessions…\n")
	case sessionPickerError:
		sb.WriteString(lipgloss.NewStyle().Foreground(dangerColor).
			Width(sessionPickerInnerWidth).
			Render("could not load sessions: " + m.errMsg))
		sb.WriteString("\n")
	case sessionPickerList:
		m.renderList(&sb)
	}

	if m.notice != "" {
		sb.WriteString(lipgloss.NewStyle().Foreground(mutedColor).Italic(true).
			Width(sessionPickerInnerWidth).Render(m.notice))
		sb.WriteString("\n")
	}

	return strings.TrimRight(sb.String(), "\n")
}

func (m *SessionPickerModel) renderList(sb *strings.Builder) {
	sb.WriteString(m.search.View())
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().
		Foreground(mutedColor).
		Render(strings.Repeat("─", sessionPickerInnerWidth)))
	sb.WriteString("\n")

	end := m.scroll + m.maxRows
	if end > len(m.filtered) {
		end = len(m.filtered)
	}
	if m.scroll > 0 {
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Width(sessionPickerInnerWidth).Render("  ↑ ···"))
		sb.WriteString("\n")
	}
	for i := m.scroll; i < end; i++ {
		style := lipgloss.NewStyle().Foreground(textColor).Width(sessionPickerInnerWidth)
		if i == m.cursor {
			style = style.Background(primaryColor).Bold(true)
		}
		sb.WriteString(style.Render(m.renderItem(m.filtered[i], i == m.cursor)))
		sb.WriteString("\n")
	}
	if end < len(m.filtered) {
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).
			Width(sessionPickerInnerWidth).Render("  ↓ ···"))
		sb.WriteString("\n")
	}
}

// renderItem builds one row: title on the left, "folder  backend  age"
// dimmed on the right. A selected row keeps its inner text unstyled so
// the cursor highlight's own foreground applies — per-part colors under
// the highlight background are unreadable.
func (m *SessionPickerModel) renderItem(item sessionPickerItem, selected bool) string {
	if item.index == sessionPickerIndexRediscover {
		label := "↻ Rediscover sessions…"
		if selected {
			return label
		}
		return lipgloss.NewStyle().Foreground(secondaryColor).Render(label)
	}
	s := m.sessions[item.index]

	suffixText := strings.TrimSpace(strings.Join([]string{
		filepath.Base(s.GitRef.LocalPath),
		string(s.Backend),
		shortTimeAgo(s.UpdatedAt),
	}, "  "))
	suffix := suffixText
	if !selected {
		suffix = lipgloss.NewStyle().Foreground(dimColor).Render(suffixText)
	}
	suffixWidth := lipgloss.Width(suffix)

	title := sessionPickerTitle(s)
	nameWidth := sessionPickerInnerWidth - suffixWidth - 2 // 2 = min gap
	if nameWidth < 4 {
		nameWidth = 4
	}
	if lipgloss.Width(title) > nameWidth {
		title = truncateToWidth(title, nameWidth-1) + "…"
	}

	gap := sessionPickerInnerWidth - lipgloss.Width(title) - suffixWidth
	if gap < 1 {
		gap = 1
	}
	return title + strings.Repeat(" ", gap) + suffix
}

// truncateToWidth cuts s to at most width terminal cells, rune-safe.
func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if lipgloss.Width(b.String()+string(r)) > width {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
