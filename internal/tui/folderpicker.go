package tui

// Folder picker modal — a cd-style directory browser for choosing the
// project folder in the compose view. You navigate into directories and
// pick one with "Use this folder". Only git repositories are selectable
// (the host can only start a session in a git repo root); non-repo folders
// stay browsable so you can navigate through them to a repo.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// folderPickerResultMsg is sent when the user selects a folder (always a
// git repo root).
type folderPickerResultMsg struct{ dir string }

// folderPickerCancelMsg is sent when the user dismisses the picker.
type folderPickerCancelMsg struct{}

type folderItemKind int

const (
	folderItemUseThis folderItemKind = iota // select the current directory
	folderItemParent                        // ".." — go up
	folderItemSubdir                        // descend into a subdirectory
)

type folderItem struct {
	kind   folderItemKind
	name   string // basename (subdir only)
	isRepo bool   // subdir is itself a git repo root
}

type folderPickerModel struct {
	current   string       // absolute path of the directory being browsed
	atRoot    bool         // current is the filesystem root (no parent)
	subdirs   []folderItem // all subdirectories of current (unfiltered)
	items     []folderItem // visible list: Use-this + ".." + filtered subdirs
	cursor    int
	scroll    int
	maxRows   int
	search    textinput.Model
	lastQuery string
	filter    string            // effective child filter (the path tail when typing a path)
	loadErr   error             // non-nil when the current directory couldn't be read
	hint      string            // transient guidance, e.g. when a non-repo is picked
	memory    map[string]string // dir path → last-highlighted subdir, so ←/→ round-trips
}

func newFolderPicker(startDir string) folderPickerModel {
	if startDir == "" || !isDir(startDir) {
		if home, err := os.UserHomeDir(); err == nil {
			startDir = home
		}
	}

	ti := textinput.New()
	ti.Placeholder = "filter, or type a /path…"
	ti.CharLimit = 128
	ti.Prompt = "/ "
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(dimColor)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)
	ti.SetWidth(52)
	ti.Focus()

	m := folderPickerModel{
		current: filepath.Clean(startDir),
		maxRows: 10,
		search:  ti,
		memory:  map[string]string{},
	}
	m.load()
	m.rebuild()
	return m
}

func (m folderPickerModel) Init() tea.Cmd {
	return func() tea.Msg { return textinput.Blink() }
}

func (m folderPickerModel) Update(msg tea.Msg) (folderPickerModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		msg = normalizeKeyCase(msg)
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			return m, func() tea.Msg { return folderPickerCancelMsg{} }

		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			return m.activate()

		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
			if m.cursor > 0 {
				m.cursor--
				m.ensureVisible()
			}
			return m, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
			if m.cursor < len(m.items)-1 {
				m.cursor++
				m.ensureVisible()
			}
			return m, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("left"))):
			// Up to the parent directory.
			if !m.atRoot {
				m.navigateTo(filepath.Dir(m.current))
			}
			return m, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("right"))):
			// Descend into the best match (or up via "..").
			if m.cursorItem().kind == folderItemParent {
				m.navigateTo(filepath.Dir(m.current))
			} else {
				m.descendIntoMatch()
			}
			return m, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+u"))):
			// Jump straight up.
			if !m.atRoot {
				m.navigateTo(filepath.Dir(m.current))
			}
			return m, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("tab", "/"))):
			// Descend one level into the best match (Tab and "/" both commit
			// the current segment and go deeper, like →).
			m.descendIntoMatch()
			return m, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("backspace", "delete"))):
			// With an empty typed segment, backspace crosses the "/" boundary:
			// step up and edit the parent's segment. Otherwise fall through to
			// the input to delete a character from the typed segment.
			if m.filter == "" {
				if !m.atRoot {
					m.backspaceAcrossBoundary()
				}
				return m, nil
			}
		}

	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			if m.cursor > 0 {
				m.cursor--
				m.ensureVisible()
			}
		case tea.MouseWheelDown:
			if m.cursor < len(m.items)-1 {
				m.cursor++
				m.ensureVisible()
			}
		}
		return m, nil

	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		return m, nil
	}

	// Forward to the input and re-evaluate on change. The input is
	// path-aware (see applyQuery), so typing a path navigates while a plain
	// word filters the current directory.
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	if q := m.search.Value(); q != m.lastQuery {
		m.lastQuery = q
		m.applyQuery(q)
	}
	return m, cmd
}

// applyQuery interprets the input. A query beginning with "/" or "~" is a
// path: it navigates to the deepest existing directory prefix and filters
// the current level by the trailing segment. Anything else filters the
// current directory's children.
func (m *folderPickerModel) applyQuery(raw string) {
	m.cursor = 0
	m.hint = ""

	if !strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "~") {
		m.filter = raw
		m.rebuild()
		return
	}

	expanded := raw
	if rest, ok := strings.CutPrefix(raw, "~"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = home + rest
		}
	}
	dirPart, tail := expanded, ""
	if i := strings.LastIndex(expanded, "/"); i >= 0 {
		dirPart, tail = expanded[:i], expanded[i+1:]
		if dirPart == "" {
			dirPart = "/"
		}
	}
	if isDir(dirPart) && filepath.Clean(dirPart) != m.current {
		m.current = filepath.Clean(dirPart)
		m.load()
	}
	m.filter = tail
	m.rebuild()
}

// setFilter sets the typed text and re-evaluates it.
func (m *folderPickerModel) setFilter(s string) {
	m.search.SetValue(s)
	m.lastQuery = s
	m.applyQuery(s)
}

// shownNextLevel is the subdirectory the breadcrumb previews as the next
// level — and therefore exactly what Tab/→/"/" descend into. Priority:
//  1. an explicitly highlighted subdir (the user moved the cursor onto it),
//  2. the typed segment's prefix match (else any substring match),
//  3. the memorized trail's head (where you last descended).
//
// Keeping this the single source of truth means "what the breadcrumb shows"
// and "what Tab does" can never diverge.
func (m folderPickerModel) shownNextLevel() (folderItem, bool) {
	if it := m.cursorItem(); it.kind == folderItemSubdir {
		return it, true
	}
	if m.filter != "" {
		var contains folderItem
		var haveContains bool
		for _, it := range m.items {
			if it.kind != folderItemSubdir {
				continue
			}
			if strings.HasPrefix(it.name, m.filter) {
				return it, true
			}
			if !haveContains {
				contains, haveContains = it, true
			}
		}
		return contains, haveContains
	}
	if t := m.memorizedTrail(); len(t) > 0 {
		for _, it := range m.items {
			if it.kind == folderItemSubdir && it.name == t[0] {
				return it, true
			}
		}
	}
	// No trail yet (e.g. just descended into a fresh folder): preview the first
	// subdirectory so Tab always suggests a next level to go deeper into.
	for _, it := range m.items {
		if it.kind == folderItemSubdir {
			return it, true
		}
	}
	return folderItem{}, false
}

// descendIntoMatch enters the previewed next level (Tab / → / "/").
func (m *folderPickerModel) descendIntoMatch() {
	if it, ok := m.shownNextLevel(); ok {
		m.navigateTo(filepath.Join(m.current, it.name))
	}
}

// backspaceAcrossBoundary steps up to the parent and seeds the typed segment
// with the folder we're leaving (minus its last rune), so backspace flows
// continuously across the "/" instead of stopping at the committed path.
//
// It routes through navigateTo so the folder we're leaving is recorded in the
// trail (memory) — exactly as ← does — otherwise the parent would have no
// previewed next level after backspacing up.
func (m *folderPickerModel) backspaceAcrossBoundary() {
	leaving := []rune(filepath.Base(m.current))
	seed := ""
	if len(leaving) > 1 {
		seed = string(leaving[:len(leaving)-1])
	}
	m.navigateTo(filepath.Dir(m.current))
	m.setFilter(seed)
}

// targetDir is the folder "Use this folder" acts on: the typed segment's
// directory when it resolves to a real one (so typing a subdir's name
// targets it), otherwise the committed current directory.
func (m folderPickerModel) targetDir() string {
	if m.filter != "" {
		if cand := filepath.Join(m.current, m.filter); isDir(cand) {
			return cand
		}
	}
	return m.current
}

// activate runs the action for the highlighted item.
func (m folderPickerModel) activate() (folderPickerModel, tea.Cmd) {
	switch it := m.cursorItem(); it.kind {
	case folderItemUseThis:
		dir := m.targetDir()
		if !dirIsGitRepo(dir) {
			m.hint = "not a git repo — open a folder that is one"
			return m, nil
		}
		return m, func() tea.Msg { return folderPickerResultMsg{dir: dir} }
	case folderItemParent:
		m.navigateTo(filepath.Dir(m.current))
	case folderItemSubdir:
		dir := filepath.Join(m.current, it.name)
		// A repo is a selectable target — Enter picks it directly (this picker
		// only ever selects repo roots). Use → to browse into it instead.
		if dirIsGitRepo(dir) {
			return m, func() tea.Msg { return folderPickerResultMsg{dir: dir} }
		}
		m.navigateTo(dir)
	}
	return m, nil
}

// navigateTo switches the browsed directory and reloads its contents,
// remembering where the cursor was so returning lands back on the folder
// you came from (a ←/→ round-trip is a no-op).
func (m *folderPickerModel) navigateTo(dir string) {
	m.rememberCursor()
	target := filepath.Clean(dir)
	// Going up: land on the folder we're leaving even if we never descended
	// into it (e.g. the picker opened directly inside it).
	if target == filepath.Dir(m.current) {
		m.memory[target] = filepath.Base(m.current)
	}
	m.current = target
	m.search.SetValue("")
	m.lastQuery = ""
	m.filter = ""
	m.hint = ""
	m.load()
	m.rebuild()
	m.restoreCursor()
}

// rememberCursor records the highlighted subdirectory for the current path.
func (m *folderPickerModel) rememberCursor() {
	if it := m.cursorItem(); it.kind == folderItemSubdir {
		m.memory[m.current] = it.name
	}
}

// restoreCursor positions the cursor on the remembered subdirectory for the
// current path, falling back to the top.
func (m *folderPickerModel) restoreCursor() {
	m.cursor = 0
	if want := m.memory[m.current]; want != "" {
		for i, it := range m.items {
			if it.kind == folderItemSubdir && it.name == want {
				m.cursor = i
				break
			}
		}
	}
	m.scroll = 0
	m.ensureVisible()
}

// load reads the current directory's subdirectories. Hidden entries
// (dot-prefixed) are skipped to keep the list focused on project folders.
func (m *folderPickerModel) load() {
	m.loadErr = nil
	m.subdirs = nil
	m.atRoot = filepath.Dir(m.current) == m.current

	entries, err := os.ReadDir(m.current)
	if err != nil {
		m.loadErr = err
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(m.current, e.Name())
		m.subdirs = append(m.subdirs, folderItem{
			kind:   folderItemSubdir,
			name:   e.Name(),
			isRepo: dirIsGitRepo(full),
		})
	}
	sort.Slice(m.subdirs, func(i, j int) bool { return m.subdirs[i].name < m.subdirs[j].name })
}

// rebuild assembles the visible item list from the current filter.
func (m *folderPickerModel) rebuild() {
	q := strings.ToLower(m.filter)
	items := []folderItem{{kind: folderItemUseThis}}
	if !m.atRoot {
		items = append(items, folderItem{kind: folderItemParent})
	}
	for _, s := range m.subdirs {
		if q == "" || strings.Contains(strings.ToLower(s.name), q) {
			items = append(items, s)
		}
	}
	m.items = items
	if m.cursor >= len(m.items) {
		m.cursor = len(m.items) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.ensureVisible()
}

func (m folderPickerModel) cursorItem() folderItem {
	if m.cursor >= 0 && m.cursor < len(m.items) {
		return m.items[m.cursor]
	}
	return folderItem{}
}

func (m *folderPickerModel) ensureVisible() {
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+m.maxRows {
		m.scroll = m.cursor - m.maxRows + 1
	}
}

// dirIsGitRepo reports whether dir is a git repo root, detected cheaply by
// the presence of a .git entry (a directory in the main worktree, a file in
// a linked worktree). The host performs the authoritative repo-root check
// when the session launches.
func dirIsGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// pickerRows sizes a modal picker's visible list to the terminal height,
// reserving room for the chrome (title, breadcrumb, separators, indicators,
// hint, border, padding) so the list fills most of the screen.
func pickerRows(height int) int {
	rows := height - 13
	if rows < 8 {
		return 8
	}
	if rows > 40 {
		return 40
	}
	return rows
}

// memorizedTrail returns the remembered subdirectory chain below the current
// directory (the path you'd retrace by descending), following the cursor
// memory. Bounded and cycle-guarded.
func (m folderPickerModel) memorizedTrail() []string {
	var trail []string
	dir := m.current
	seen := map[string]bool{dir: true}
	for len(trail) < 12 {
		next := m.memory[dir]
		if next == "" {
			break
		}
		dir = filepath.Join(dir, next)
		if seen[dir] || !isDir(dir) {
			break
		}
		seen[dir] = true
		trail = append(trail, next)
	}
	return trail
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(filepath.Clean(p), string(filepath.Separator)) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// renderBreadcrumb renders the path. While typing, the committed path is
// context and the typed text is the pending next segment (highlighted, with a
// cursor) — there is no separate input line. At rest it highlights the current
// level and appends the memorized deeper trail dimmed. Ancestors truncate from
// the left so the important tail stays visible.
func (m folderPickerModel) renderBreadcrumb(width int) string {
	anc := lipgloss.NewStyle().Foreground(secondaryColor)
	cur := lipgloss.NewStyle().Foreground(primaryColor).Bold(true)
	dim := lipgloss.NewStyle().Foreground(dimColor)

	if m.filter != "" {
		// The committed path is context; the typed text is the pending segment
		// with a cursor block. A ghost (the rest of the best match) shows what
		// Tab/→ would complete to; no match at all turns the segment red.
		prefix := strings.TrimSuffix(m.current, "/") + "/"
		ghost := ""
		seg := cur
		if next, ok := m.shownNextLevel(); !ok {
			seg = lipgloss.NewStyle().Foreground(dangerColor).Bold(true)
		} else if strings.HasPrefix(next.name, m.filter) {
			ghost = next.name[len(m.filter):]
		}
		prefix = truncateLeft(prefix, width-lipgloss.Width(m.filter)-lipgloss.Width(ghost)-1)
		return anc.Render(prefix) + seg.Render(m.filter) + cur.Render("█") + dim.Render(ghost)
	}

	parts := splitPath(m.current)
	last, pre := "", "/"
	if len(parts) > 0 {
		last = parts[len(parts)-1]
		if len(parts) > 1 {
			pre = "/" + strings.Join(parts[:len(parts)-1], "/") + "/"
		}
	}
	// Preview the next level the same way Tab descends: the highlighted subdir,
	// or the memorized trail. Show the deeper trail too when it starts there.
	suffix := ""
	if next, ok := m.shownNextLevel(); ok {
		segs := []string{next.name}
		if t := m.memorizedTrail(); len(t) > 0 && t[0] == next.name {
			segs = t
		}
		suffix = strings.Join(segs, "/")
	}
	pre = truncateLeft(pre, width-lipgloss.Width(last)-lipgloss.Width(suffix)-2)
	// The cursor sits just past the separator: we're picking the folder *under*
	// the current level, so the cursor leads the child segment.
	return anc.Render(pre) + cur.Render(last) + dim.Render("/") + cur.Render("█") + dim.Render(suffix)
}

// truncateLeft trims s from the left to fit budget columns, prefixing "…".
func truncateLeft(s string, budget int) string {
	if budget < 0 {
		budget = 0
	}
	if lipgloss.Width(s) <= budget {
		return s
	}
	r := []rune(s)
	if budget > 1 {
		return "…" + string(r[len(r)-(budget-1):])
	}
	return ""
}

func (m folderPickerModel) View() string {
	const menuWidth = 60
	innerWidth := menuWidth - 4

	var sb strings.Builder

	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(textColor).Width(innerWidth).Render("Select project folder"))
	sb.WriteString("\n")

	// Breadcrumb doubles as the input: the typed text appears as the pending
	// next segment (highlighted, with a cursor); when empty it shows the
	// current level highlighted plus the dimmed memorized trail.
	sb.WriteString(m.renderBreadcrumb(innerWidth))
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(mutedColor).Render(strings.Repeat("─", innerWidth)))
	sb.WriteString("\n")

	// Fixed-height block: a reserved top/bottom indicator line, exactly
	// maxRows item lines (blank-padded), and a reserved hint line. Keeping the
	// height constant stops the centered modal from jumping as results change.
	blank := lipgloss.NewStyle().Width(innerWidth).Render(" ")
	indicator := func(s string) string {
		return lipgloss.NewStyle().Foreground(dimColor).Width(innerWidth).Render(s)
	}
	end := m.scroll + m.maxRows
	if end > len(m.items) {
		end = len(m.items)
	}

	if m.scroll > 0 {
		sb.WriteString(indicator("  ↑ ···"))
	} else {
		sb.WriteString(blank)
	}
	sb.WriteString("\n")

	rows := 0
	switch {
	case m.loadErr != nil:
		sb.WriteString(lipgloss.NewStyle().Foreground(dangerColor).Width(innerWidth).Render("  cannot read folder"))
		sb.WriteString("\n")
		rows++
	case len(m.items) == 0:
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Width(innerWidth).Render("  (empty)"))
		sb.WriteString("\n")
		rows++
	default:
		for i := m.scroll; i < end; i++ {
			isLast := i == len(m.items)-1
			selected := i == m.cursor
			label := m.renderItem(m.items[i], isLast, selected)
			if selected {
				sb.WriteString(lipgloss.NewStyle().Background(primaryColor).Foreground(textColor).Bold(true).Width(innerWidth).Render(label))
			} else {
				sb.WriteString(lipgloss.NewStyle().Width(innerWidth).Render(label))
			}
			sb.WriteString("\n")
			rows++
		}
	}
	for ; rows < m.maxRows; rows++ {
		sb.WriteString(blank)
		sb.WriteString("\n")
	}

	if end < len(m.items) {
		sb.WriteString(indicator("  ↓ ···"))
	} else {
		sb.WriteString(blank)
	}
	sb.WriteString("\n")

	hint := "type/tab to complete · ↑↓ move · ←→ nav · enter · esc"
	sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Width(innerWidth).Render(hint))
	sb.WriteString("\n")
	if m.hint != "" {
		sb.WriteString(lipgloss.NewStyle().Foreground(warningColor).Width(innerWidth).Render(m.hint))
	} else {
		sb.WriteString(blank)
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())
}

// renderItem builds the display string for one row. Directories render as
// a tree (├─ / └─); a green ● on the left marks git repos, and non-repos
// are dimmed. The selected row is returned plain so the cursor highlight
// applied by View stays legible.
func (m folderPickerModel) renderItem(it folderItem, isLast, selected bool) string {
	if it.kind == folderItemUseThis {
		repo := dirIsGitRepo(m.targetDir())
		text := "✗ Use this folder (not a git repo)"
		if repo {
			text = "✓ Use this folder"
		}
		if selected {
			return text
		}
		c := dimColor
		if repo {
			c = successColor
		}
		return lipgloss.NewStyle().Foreground(c).Render(text)
	}

	conn := "├─ "
	if isLast {
		conn = "└─ "
	}
	label := ".."
	if it.kind == folderItemSubdir {
		label = it.name + "/"
	}

	// The 2-cell marker slot keeps every label left-aligned whether or not
	// the folder is a repo.
	if selected {
		marker := "  "
		if it.isRepo {
			marker = "● "
		}
		return conn + marker + label
	}
	connStr := lipgloss.NewStyle().Foreground(mutedColor).Render(conn)
	if it.isRepo {
		return connStr +
			lipgloss.NewStyle().Foreground(successColor).Render("● ") +
			lipgloss.NewStyle().Foreground(textColor).Render(label)
	}
	return connStr + "  " + lipgloss.NewStyle().Foreground(dimColor).Render(label)
}
