package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
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

	var lines []string
	lines = append(lines,
		lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("Worktrees"),
		"",
	)
	bodyLines, footerStart := m.renderBody(contentWidth)
	lines = append(lines, bodyLines...)

	if m.err != nil {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(dangerColor).
			Render(truncateStr(m.err.Error(), contentWidth)))
	}

	listH := m.listHeight()
	if pad := listH - len(lines) - footerStart; pad > 0 {
		for i := 0; i < pad; i++ {
			lines = append(lines, "")
		}
	}
	lines = append(lines, m.renderFooterSection(contentWidth)...)

	content := strings.Join(lines, "\n")
	style := sidebarBorderStyle(m.focused).Width(w - paneBorderInset).Height(listH)
	return style.Render(content)
}

// renderBody emits the non-footer rows (AllSessions + worktrees + older
// bucket), respecting the scroll offset. footerLineCount is returned so
// View can pad to push the footer to the bottom regardless of how many
// rows the body produced.
func (m *SidebarModel) renderBody(contentWidth int) (lines []string, footerLineCount int) {
	footerLineCount = 4 // separator + 3 footer rows
	body, _ := m.bodyNodes()
	visible := m.visibleBodyNodes(body)

	hasContent := false
	for _, idx := range visible {
		n := m.flat[idx]
		selected := idx == m.cursor && m.focused
		// Insert a dim rule before every top-level row (worktree or
		// top-level overflow bucket) whenever any body row already
		// rendered. That covers both the divider between consecutive
		// worktrees and the divider between the AllSessions header
		// and the first worktree.
		if (n.Kind() == nodeWorktree || n.Kind() == nodeOlderWorktrees) && hasContent {
			lines = append(lines, m.renderWorktreeSeparator(contentWidth))
		}
		switch typed := n.(type) {
		case allSessionsNode:
			lines = append(lines, m.renderAllRow(selected, contentWidth))
		case worktreeNode:
			lines = append(lines, m.renderWorktreeRow(typed, idx, selected, contentWidth))
		case sessionNode:
			lines = append(lines, m.renderSessionRow(typed, selected, contentWidth)...)
		case olderWorktreesNode:
			lines = append(lines, m.renderOlderWorktreesRow(typed, selected, contentWidth))
		case olderSessionsNode:
			lines = append(lines, m.renderOlderSessionsRow(typed, selected, contentWidth))
		}
		hasContent = true
	}
	return lines, footerLineCount
}

// bodyNodes splits the flat list into the indices of body rows
// (everything before the footer) and the index where the footer starts.
// Cached helper for both rendering and scroll math.
func (m *SidebarModel) bodyNodes() (body []int, footerStart int) {
	for i, n := range m.flat {
		switch n.Kind() {
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
// fixed-overhead rows: header(1) + blank(1) at the top, footer
// separator + 3 footer rows at the bottom = 6. When creating mode is
// active the input also lives in the body (accounted for in
// nodeLineCost), so no extra deduction is needed here.
func (m *SidebarModel) bodyViewportH() int {
	const overhead = 2 + 4 // header block + footer block
	vh := m.listHeight() - overhead
	if vh < 1 {
		vh = 1
	}
	return vh
}

// listHeight returns the height available for the body (excluding border).
func (m *SidebarModel) listHeight() int {
	h := m.height - 4 // border top/bottom + some padding
	if h < 5 {
		h = 5
	}
	return h
}

// renderAllRow renders the virtual "All sessions" entry pinned at the
// top of the body. Uses the same right-edge cursor marker as worktree
// rows so the "you are here" signal stays consistent across the
// whole tree — the label sits at the left margin, selection only
// adds a bold weight and the right-edge ◀.
func (m *SidebarModel) renderAllRow(selected bool, maxWidth int) string {
	label := "  All sessions"
	style := lipgloss.NewStyle().Foreground(dimColor)
	if m.cursor == 0 {
		style = lipgloss.NewStyle().Foreground(textColor)
	}
	if selected {
		style = style.Bold(true)
	}
	line := style.Render(label)
	if selected {
		line = padRight(line, maxWidth-rightCursorWidth) + " " +
			lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(rightCursorGlyph)
	}
	return line
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
//	 >label<  ☁           <repoLabel>  ◀     (collapsed)
//	< label >  ☁          <repoLabel>  ◀     (expanded)
//
// Collapsed reads as "closed and compact": the brackets clamp the
// name with no inner space, and an outer space pads them. Expanded
// reads as "open and breathing": brackets push outward with inner
// padding, no outer space. Both variants line the label up at the
// same column so adjacent rows still align.
func (m *SidebarModel) renderWorktreeRow(n worktreeNode, idx int, selected bool, maxWidth int) string {
	// Collapsed: ` >name< ` (outer pad, inner tight).
	// Expanded:  `< name >` (outer tight, inner pad).
	leftPrefix, rightSuffix := " >", "< "
	if m.expanded[n.Key()] {
		leftPrefix, rightSuffix = "< ", " >"
	}

	ownerGlyph := ""
	if n.OwnerKind == "remote" {
		ownerGlyph = " " + lipgloss.NewStyle().Foreground(primaryColor).Render("☁")
	}

	cursorReserve := 0
	if selected {
		cursorReserve = rightCursorWidth
	}
	repoTagWidth := 0
	if n.RepoLabel != "" {
		repoTagWidth = len(n.RepoLabel) + 1 // one space gutter
	}

	// Reserve room for: prefix (2) + suffix (2) + owner glyph + repo tag + cursor.
	maxLabel := maxWidth - 4 - repoTagWidth - cursorReserve - lipgloss.Width(ownerGlyph)
	if maxLabel < 6 {
		maxLabel = 6
	}
	label := n.Label
	if len(label) > maxLabel {
		label = label[:maxLabel-1] + "…"
	}

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

	line := nameStyle.Render(leftPrefix+label+rightSuffix) + ownerGlyph

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
	const markerWidth = 2 // marker glyph + trailing space
	title := sessionTitle(n.Session)
	marker := sessionMarker(n.Session)
	active := n.Session.ID != "" && n.Session.ID == m.activeSessionID

	cursorReserve := 0
	if selected {
		cursorReserve = rightCursorWidth
	}

	maxTitle := maxWidth - len(indent) - markerWidth - cursorReserve
	if maxTitle < 10 {
		maxTitle = 10
	}
	title = truncateStr(title, maxTitle)

	// All read sessions keep textColor; unread sessions are bold so the
	// row stands out against the dim age line below it. Done/archived
	// stay muted to signal the row is no longer "live".
	titleStyle := lipgloss.NewStyle().Foreground(textColor)
	switch {
	case n.Session.Visibility == agent.VisibilityArchived:
		titleStyle = titleStyle.Foreground(mutedColor)
	case n.Session.Visibility == agent.VisibilityDone:
		titleStyle = titleStyle.Foreground(dimColor)
	case sessionTitleIsFallback(n.Session):
		titleStyle = titleStyle.Foreground(dimColor)
	case selected, n.Session.Unread():
		titleStyle = titleStyle.Bold(true)
	}

	titleLeft := m.leftRailOrIndent(active, indent)
	line1 := titleLeft + marker + " " + titleStyle.Render(title)
	if selected {
		line1 = padRight(line1, maxWidth-rightCursorWidth) + " " +
			lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(rightCursorGlyph)
	}

	// Age line shares the title's column so it visually stacks under
	// the title text. The rail (when active) lives in the same column
	// on both lines for an unbroken visual mark.
	ageLeft := m.leftRailOrIndent(active, indent) + strings.Repeat(" ", markerWidth)
	timeLine := lipgloss.NewStyle().Foreground(dimColor).Render(ageLeft + shortTimeAgo(n.Session.UpdatedAt))
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
