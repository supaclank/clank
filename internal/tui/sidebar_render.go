package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/supaclank/clank/internal/agent"
)

// View renders the sidebar by emitting one or more terminal rows per
// sidebarNode. Worktree and bucket rows render on a single line;
// session rows render on two (title + relative time). The cursor's row
// is highlighted with the standard "> " selection prefix.
func (m *SidebarModel) View() string {
	w := m.width
	if w <= 0 {
		w = sidebarWidth
	}

	// lipgloss v2 includes border in the Width(), so subtract the
	// inset; further -2 keeps double-width glyphs (e.g. ⚙) from
	// wrapping when terminal widths land on edge cases.
	contentWidth := w - 4
	if contentWidth < 12 {
		contentWidth = 12
	}

	// flat is built in lockstep with lines: flat[i] is the node index
	// drawn on line i, or noNodeRow for the header / blanks / padding.
	// NodeAtRow consumes it to resolve clicks.
	var lines []string
	var flat []int
	add := func(node int, ls ...string) {
		for _, l := range ls {
			lines = append(lines, l)
			for n := lipgloss.Height(l); n > 0; n-- {
				flat = append(flat, node)
			}
		}
	}

	add(noNodeRow, lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("Home"))
	homeIndex := 0
	if len(m.flat) == 0 || m.flat[0].Kind() != nodeHome {
		homeIndex = noNodeRow
	}
	add(homeIndex, m.renderHomeRow(homeIndex == m.cursor && m.focused, contentWidth))
	add(noNodeRow,
		"",
		lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("Worktrees"),
		"",
	)
	bodyLines, bodyFlat, footerLineCount := m.renderBody(contentWidth)
	lines = append(lines, bodyLines...)
	flat = append(flat, bodyFlat...)

	if m.err != nil {
		add(noNodeRow, "", lipgloss.NewStyle().Foreground(dangerColor).
			Render(truncateStr(m.err.Error(), contentWidth)))
	}

	listH := m.listHeight()
	if pad := listH - len(lines) - footerLineCount; pad > 0 {
		for i := 0; i < pad; i++ {
			add(noNodeRow, "")
		}
	}
	// The footer is always [separator, Import, Cloud, Settings] — the last
	// three flat nodes (flattenSidebar appends them in that order).
	footerLines := m.renderFooterSection(contentWidth)
	footerBase := len(m.flat) - 3
	add(noNodeRow, footerLines[0]) // separator
	for i, l := range footerLines[1:] {
		node := noNodeRow
		if footerBase >= 0 {
			node = footerBase + i
		}
		add(node, l)
	}

	m.rowFlat = flat

	content := strings.Join(lines, "\n")
	style := sidebarBorderStyle(m.focused).Width(w - paneBorderInset).Height(listH)
	return style.Render(content)
}

// noNodeRow marks a rendered line that maps to no selectable node
// (header, blank spacer, padding, footer separator).
const noNodeRow = -1

// sidebarTopBorderRows is the number of screen rows the sidebar's top
// border occupies above its first content line. Mouse Y is offset by
// this to index into rowFlat.
const sidebarTopBorderRows = 1

// NodeAtRow maps a sidebar-local screen row (mouse Y — the sidebar
// occupies the left columns starting at row 0) to the flat node index
// rendered there, or -1 when the row is a border / blank / separator or
// out of range. Reads the map cached by the last View().
func (m *SidebarModel) NodeAtRow(y int) int {
	line := y - sidebarTopBorderRows
	if line < 0 || line >= len(m.rowFlat) {
		return -1
	}
	return m.rowFlat[line]
}

// renderBody emits the non-footer rows below Home (worktrees + older bucket),
// respecting the scroll offset. footerLineCount is returned so
// View can pad to push the footer to the bottom regardless of how many
// rows the body produced.
func (m *SidebarModel) renderBody(contentWidth int) (lines []string, flatByLine []int, footerLineCount int) {
	footerLineCount = 4 // separator + 3 footer rows
	body, _ := m.bodyNodes()
	visible := m.visibleBodyNodes(body)

	// emit appends one rendered row and records the owning flat index for
	// every screen line it occupies. A single row string can wrap to
	// several lines — a session row is title + time, and the title can
	// itself carry a newline — so the index is repeated per rendered line
	// (lipgloss.Height) to keep clicks below it aligned.
	emit := func(idx int, rows ...string) {
		for _, r := range rows {
			lines = append(lines, r)
			for n := lipgloss.Height(r); n > 0; n-- {
				flatByLine = append(flatByLine, idx)
			}
		}
	}

	hasContent := false
	for _, idx := range visible {
		n := m.flat[idx]
		selected := idx == m.cursor && m.focused
		// Insert a dim rule before every top-level row (worktree or
		// top-level overflow bucket) whenever any body row already
		// rendered — the divider between consecutive worktrees. The
		// rule is attributed to the row it precedes so clicking the
		// thin divider still selects it.
		if (n.Kind() == nodeWorktree || n.Kind() == nodeOlderWorktrees) && hasContent {
			emit(idx, m.renderWorktreeSeparator(contentWidth))
		}
		switch typed := n.(type) {
		case worktreeNode:
			emit(idx, m.renderWorktreeRow(typed, idx, selected, contentWidth))
		case sessionNode:
			emit(idx, m.renderSessionRow(typed, selected, contentWidth)...)
		case olderWorktreesNode:
			emit(idx, m.renderOlderWorktreesRow(typed, selected, contentWidth))
		case olderSessionsNode:
			emit(idx, m.renderOlderSessionsRow(typed, selected, contentWidth))
		}
		hasContent = true
	}
	return lines, flatByLine, footerLineCount
}

// bodyNodes splits the flat list into the indices of body rows
// (everything before the footer) and the index where the footer starts.
// Cached helper for both rendering and scroll math.
func (m *SidebarModel) bodyNodes() (body []int, footerStart int) {
	for i, n := range m.flat {
		switch n.Kind() {
		case nodeHome:
			continue
		case nodeImport, nodeCloud, nodeSettings:
			if footerStart == 0 {
				footerStart = i
			}
		default:
			body = append(body, i)
		}
	}
	if footerStart == 0 {
		footerStart = len(m.flat)
	}
	return body, footerStart
}

// visibleBodyNodes returns the subset of body indices that fit in the
// scroll viewport in terms of *rendered terminal lines*, not raw node
// count. Session rows render two lines, worktree/older rows add one
// separator line each (except when first in the visible window), and
// the inline new-branch input adds one line when active. Counting
// in lines is the only way to keep the body inside its allotted
// vertical space — naive node counting overflowed and pushed the
// footer outside the bordered Height (silently truncating it).
//
// The cursor-visibility check uses the same line accounting so a
// session row near the bottom gets two visible rows of room, not one.
func (m *SidebarModel) visibleBodyNodes(body []int) []int {
	vh := m.bodyViewportH()
	if vh <= 0 || len(body) == 0 {
		return nil
	}

	cursorBodyIdx := -1
	for i, idx := range body {
		if idx == m.cursor {
			cursorBodyIdx = i
			break
		}
	}

	if m.scroll < 0 {
		m.scroll = 0
	}
	if m.scroll > len(body)-1 {
		m.scroll = len(body) - 1
	}

	if cursorBodyIdx >= 0 {
		// Snap up if the cursor scrolled off the top.
		if cursorBodyIdx < m.scroll {
			m.scroll = cursorBodyIdx
		}
		// Advance scroll forward until the cursor's row fits inside vh.
		// Each iteration shifts the window by one body node.
		for m.scroll < cursorBodyIdx && m.linesFromTo(body, m.scroll, cursorBodyIdx) > vh {
			m.scroll++
		}
	}

	// Collect nodes from m.scroll forward, stopping when the next
	// node would push us past vh.
	visible := make([]int, 0, len(body)-m.scroll)
	used := 0
	for i := m.scroll; i < len(body); i++ {
		cost := m.nodeLineCost(body[i], i == m.scroll)
		if used+cost > vh {
			break
		}
		visible = append(visible, body[i])
		used += cost
	}
	return visible
}

// linesFromTo sums the rendered line cost of body[from..to] inclusive,
// treating body[from] as the first visible row (so its separator
// doesn't count). Used by the scroll math to decide whether the cursor
// can fit inside the viewport with the current scroll offset.
func (m *SidebarModel) linesFromTo(body []int, from, to int) int {
	total := 0
	for i := from; i <= to; i++ {
		total += m.nodeLineCost(body[i], i == from)
	}
	return total
}

// nodeLineCost returns how many terminal lines the body node at flat
// index `idx` takes when rendered, including the inter-worktree
// separator that precedes it (when applicable) and the inline
// new-branch input (when creating mode parks on a worktree row).
// isFirstVisible suppresses the separator for the top of the viewport,
// matching renderBody's hasContent-gated separator emission.
func (m *SidebarModel) nodeLineCost(idx int, isFirstVisible bool) int {
	n := m.flat[idx]
	cost := 1
	if n.Kind() == nodeSession {
		cost = 2
	}
	if !isFirstVisible && (n.Kind() == nodeWorktree || n.Kind() == nodeOlderWorktrees) {
		cost++ // separator line emitted before this row
	}
	return cost
}

// bodyViewportH returns the number of rendered terminal lines the
// body section can fill. The body shares listHeight with the
// fixed-overhead rows: "Home" + home row + blank + "Worktrees" + blank
// (5) at the top, footer separator + 3 footer rows (4) at the bottom.
// When creating mode is active the input also lives in the body
// (accounted for in nodeLineCost), so no extra deduction is needed here.
func (m *SidebarModel) bodyViewportH() int {
	const overhead = 5 + 4 // Home/Worktrees header block + footer block
	vh := m.listHeight() - overhead
	if vh < 1 {
		vh = 1
	}
	return vh
}

// renderHomeRow renders the fixed link to the welcome screen. The
// right-edge cursor chevron (◀) tracks the keyboard cursor only, like
// worktree rows. A primary-colored left rail (▎) marks the row while
// the welcome screen is the active right-pane view — matching the
// active-session rail — so "what's open" stays visible even after the
// cursor moves elsewhere in the sidebar.
func (m *SidebarModel) renderHomeRow(selected bool, maxWidth int) string {
	const label = "⌂ Home"
	cursorHere := selected && m.focused
	// The ▎ rail is a single "what's open" marker shared with sessions;
	// never paint it on Home while a session also claims it.
	homeRail := m.homeActive && m.activeSessionID == ""

	nameStyle := lipgloss.NewStyle().Foreground(dimColor)
	if cursorHere || homeRail {
		nameStyle = lipgloss.NewStyle().Foreground(textColor).Bold(true)
	}

	rail := " "
	if homeRail {
		rail = lipgloss.NewStyle().Foreground(primaryColor).Render(activeSessionRail)
	}
	line := rail + nameStyle.Render(label)

	if cursorHere {
		line = padRight(line, maxWidth-rightCursorWidth) + " " +
			lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(rightCursorGlyph)
	}
	return line
}

// listHeight returns the height available for the body (excluding border).
func (m *SidebarModel) listHeight() int {
	h := m.height - 4 // border top/bottom + some padding
	if h < 5 {
		h = 5
	}
	return h
}

// renderInputRow renders the inline new-branch input.
// rightCursorGlyph is the selection marker appended to the right edge
// of chevron-bearing rows (worktree + Older buckets). Keeps the chevron
// fixed at column 0 so the expand/collapse indicator stops shifting
// horizontally when the cursor lands on a row.
const rightCursorGlyph = "◀"

// rightCursorWidth is the visible width of " " + rightCursorGlyph.
const rightCursorWidth = 2

// renderWorktreeRow renders one worktree as a single line. Layout:
//
//	▸ label              <repoLabel>  ◀     (collapsed)
//	▾ label              <repoLabel>  ◀     (expanded)
//
// A single chevron sits to the left of the name and flips direction to
// show expand/collapse state, matching the accordion chevrons used
// elsewhere in the sidebar (e.g. the Done/Archive toggles).
func (m *SidebarModel) renderWorktreeRow(n worktreeNode, idx int, selected bool, maxWidth int) string {
	chevron := "▸"
	if m.expanded[n.Key()] {
		chevron = "▾"
	}

	cursorReserve := 0
	if selected {
		cursorReserve = rightCursorWidth
	}
	repoTagWidth := 0
	if n.RepoLabel != "" {
		repoTagWidth = lipgloss.Width(n.RepoLabel) + 1 // one space gutter
	}

	// Reserve room for: chevron (1) + space (1) + repo tag + cursor.
	maxLabel := maxWidth - 2 - repoTagWidth - cursorReserve
	if maxLabel < 6 {
		maxLabel = 6
	}
	label := truncateStr(n.Label, maxLabel)

	// Pick the base color from the row's state (archived / done /
	// expanded / default), then add bold on top when the cursor is
	// on the row. Selection only adds bold — it never re-colors —
	// so an expanded row stays teal whether or not the cursor is
	// parked on it. The right-edge cursor marker carries the
	// "you are here" signal on its own.
	nameStyle := lipgloss.NewStyle().Foreground(textColor)
	switch {
	case n.Archived == n.Total && n.Total > 0:
		nameStyle = nameStyle.Foreground(mutedColor)
	case n.Active == 0 && n.Total > 0:
		nameStyle = nameStyle.Foreground(dimColor)
	case m.expanded[n.Key()]:
		nameStyle = nameStyle.Foreground(secondaryColor)
	}
	if selected {
		nameStyle = nameStyle.Bold(true)
	}

	line := nameStyle.Render(chevron + " " + label)

	if n.RepoLabel != "" {
		repoStyle := lipgloss.NewStyle().Foreground(mutedColor)
		line = padRight(line, maxWidth-repoTagWidth-cursorReserve) + repoStyle.Render(n.RepoLabel)
	}
	if selected {
		line = padRight(line, maxWidth-rightCursorWidth) + " " +
			lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(rightCursorGlyph)
	}
	return line
}

// sidebarChildIndent is the left margin every nested row (sessions,
// per-worktree overflow bucket) shares so they line up under their
// parent worktree.
const sidebarChildIndent = "  "

// activeSessionRail is the glyph rendered at column 0 of both lines of
// the session row that's currently shown in the right pane. Half-width
// so it slots into one of sidebarChildIndent's leading spaces without
// pushing the title or age over. Lives separately from the cursor
// indicator (◀) so "what's open" and "where my keys land" can both
// show without competing for one column.
const activeSessionRail = "▎"

// renderSessionRow returns two lines per session: title (preceded by
// the unread/follow-up marker) and the short relative time below.
// Indentation is the same for both lines so they share a left margin.
// When the row is the active session (currently shown in the right
// pane), a primary-colored rail replaces the leading space on both
// lines.
func (m *SidebarModel) renderSessionRow(n sessionNode, selected bool, maxWidth int) []string {
	const indent = sidebarChildIndent
	const indicatorWidth = 2 // glyph + trailing space
	title := m.renderedTitleFor(n.Session.ID, sessionTitle(n.Session))
	active := n.Session.ID != "" && n.Session.ID == m.activeSessionID

	// One combined indicator column. Precedence: busy/starting spinner
	// outranks the unread/follow-up marker so an actively-working
	// session shows the spinner; the title still bolds on unread to
	// keep the unread signal. Idle sessions fall through to the marker
	// (! follow-up, * unread, blank otherwise).
	indicator := sessionMarker(n.Session)
	if n.Session.Status == agent.StatusBusy || n.Session.Status == agent.StatusStarting {
		indicator = styledAgentStatus(n.Session.Status, m.spinnerFrame)
	}

	cursorReserve := 0
	if selected {
		cursorReserve = rightCursorWidth
	}

	maxTitle := maxWidth - len(indent) - indicatorWidth - cursorReserve
	if maxTitle < 10 {
		maxTitle = 10
	}
	title = truncateStr(title, maxTitle)

	// Title color tracks visibility / fallback state; bold layers on
	// for hover or unread so both signals compose cleanly.
	titleStyle := lipgloss.NewStyle().Foreground(textColor)
	switch {
	case n.Session.Visibility == agent.VisibilityArchived:
		titleStyle = titleStyle.Foreground(mutedColor)
	case n.Session.Visibility == agent.VisibilityDone:
		titleStyle = titleStyle.Foreground(dimColor)
	case sessionTitleIsFallback(n.Session):
		titleStyle = titleStyle.Foreground(dimColor)
	}
	if selected || n.Session.Unread() {
		titleStyle = titleStyle.Bold(true)
	}

	titleLeft := m.leftRailOrIndent(active, indent)
	line1 := titleLeft + indicator + " " + titleStyle.Render(title)
	if selected {
		line1 = padRight(line1, maxWidth-rightCursorWidth) + " " +
			lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(rightCursorGlyph)
	}

	// Age line stacks under the title. Rail concatenation stays OUTSIDE
	// the styled wrap — its SGR reset would otherwise kill the dim
	// foreground on the time text (see TestRenderSessionRow_AgeLineColor).
	ageBody := lipgloss.NewStyle().Foreground(dimColor).
		Render(strings.Repeat(" ", indicatorWidth) + shortTimeAgo(n.Session.UpdatedAt))
	timeLine := m.leftRailOrIndent(active, indent) + ageBody
	return []string{line1, timeLine}
}

// leftRailOrIndent returns the same-width left margin used by nested
// rows, swapping the leading space for the active-session rail when
// active is true. The rail uses primaryColor and stays rendered
// regardless of pane focus — it marks "what's open," not "where the
// cursor is."
func (m *SidebarModel) leftRailOrIndent(active bool, indent string) string {
	if !active || len(indent) == 0 {
		return indent
	}
	return lipgloss.NewStyle().Foreground(primaryColor).Render(activeSessionRail) + indent[1:]
}

// renderOlderWorktreesRow renders the collapsible top-level overflow
// bucket. Reads as "+ show more (N)" when collapsed and "− show less"
// when expanded so the affordance matches what activating the row does.
func (m *SidebarModel) renderOlderWorktreesRow(n olderWorktreesNode, selected bool, maxWidth int) string {
	return m.renderBucketRow(n.Key(), bucketLabel(m.expanded[n.Key()], len(n.Hidden)), selected, "", maxWidth)
}

// renderOlderSessionsRow renders the per-worktree overflow bucket,
// aligned with the session rows under the same worktree.
func (m *SidebarModel) renderOlderSessionsRow(n olderSessionsNode, selected bool, maxWidth int) string {
	return m.renderBucketRow(n.Key(), bucketLabel(m.expanded[n.Key()], len(n.Hidden)), selected, sidebarChildIndent, maxWidth)
}

// bucketLabel returns the human label for an overflow bucket. Reuses
// the same wording for both the worktree-level and session-level
// buckets so the affordance stays uniform.
func bucketLabel(expanded bool, hiddenCount int) string {
	if expanded {
		return "− show less"
	}
	return fmt.Sprintf("+ show more (%d)", hiddenCount)
}

// renderBucketRow is the shared layout for overflow buckets at either
// depth: a muted "show more / show less" affordance. Selection is
// indicated by a right-edge marker so the +/- glyph stays put.
func (m *SidebarModel) renderBucketRow(_ string, label string, selected bool, indent string, maxWidth int) string {
	labelStyle := lipgloss.NewStyle().Foreground(mutedColor)
	if selected {
		labelStyle = lipgloss.NewStyle().Foreground(textColor).Bold(true)
	}
	line := indent + labelStyle.Render(label)
	if selected {
		line = padRight(line, maxWidth-rightCursorWidth) + " " +
			lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(rightCursorGlyph)
	}
	return line
}

// renderWorktreeSeparator renders the dim horizontal rule emitted
// between consecutive worktree blocks (and before the top-level Older
// bucket). Reuses the same glyph as the footer divider so the sidebar
// has one consistent separator style.
func (m *SidebarModel) renderWorktreeSeparator(width int) string {
	return lipgloss.NewStyle().Foreground(mutedColor).Render(strings.Repeat("─", width))
}

// renderFooterSection renders the divider plus three footer rows.
func (m *SidebarModel) renderFooterSection(maxWidth int) []string {
	sep := lipgloss.NewStyle().Foreground(mutedColor).Render(strings.Repeat("─", maxWidth))
	return []string{
		sep,
		m.renderFooterRow("↓ Import Sessions", m.CursorOnImport()),
		m.renderCloudFooterRow(),
		m.renderFooterRow("⚙ Settings", m.CursorOnSettings()),
	}
}

// renderCloudFooterRow renders the "☁ Cloud" row with the connection
// indicator placed inline immediately after the label.
func (m *SidebarModel) renderCloudFooterRow() string {
	indicator := m.renderCloudStatusIndicator()
	if indicator == "" {
		return m.renderFooterRow("☁ Cloud", m.CursorOnCloud())
	}
	return m.renderFooterRow("☁ Cloud "+indicator, m.CursorOnCloud())
}

// renderCloudStatusIndicator renders the small dot+label shown on the
// cloud footer row. Returns "" when no cloud_url is configured.
func (m *SidebarModel) renderCloudStatusIndicator() string {
	switch m.cloudStatus {
	case cloudStatusOnline:
		return lipgloss.NewStyle().Foreground(successColor).Render("●")
	case cloudStatusChecking:
		glyph := m.cloudSpinnerFrame
		if glyph == "" {
			glyph = "◌"
		}
		return lipgloss.NewStyle().Foreground(secondaryColor).Render(glyph)
	case cloudStatusUnavailable:
		return lipgloss.NewStyle().Foreground(dangerColor).Render("●")
	case cloudStatusOffline:
		return lipgloss.NewStyle().Foreground(dimColor).Render("○")
	default:
		return ""
	}
}

// renderFooterRow renders one footer row with hover/selected styling.
func (m *SidebarModel) renderFooterRow(label string, cursorOn bool) string {
	if cursorOn && m.focused {
		prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		return prefix + lipgloss.NewStyle().Foreground(textColor).Bold(true).Render(label)
	}
	if cursorOn {
		return lipgloss.NewStyle().Foreground(textColor).Render("  " + label)
	}
	return lipgloss.NewStyle().Foreground(dimColor).Render("  " + label)
}

// padRight pads `s` (ANSI-aware visible width) with spaces so its
// printed width equals `width`. Strings already longer are returned
// unchanged.
func padRight(s string, width int) string {
	vw := lipgloss.Width(s)
	if vw >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vw)
}
