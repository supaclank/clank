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
	body, footerStart := m.bodyNodes()
	visible := m.visibleBodyNodes(body, footerStart)

	hasContent := false
	for _, idx := range visible {
		n := m.flat[idx]
		selected := idx == m.cursor && m.focused && !m.creating
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
			lines = append(lines, m.renderAllRow(selected))
		case worktreeNode:
			lines = append(lines, m.renderWorktreeRow(typed, idx, selected, contentWidth))
			if m.creating && idx == m.cursor {
				m.input.SetWidth(contentWidth - 2)
				lines = append(lines, m.renderInputRow())
			}
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
// scroll viewport, anchored so the cursor stays visible.
func (m *SidebarModel) visibleBodyNodes(body []int, footerStart int) []int {
	vh := m.bodyViewportH()
	if vh <= 0 || len(body) == 0 {
		return nil
	}
	// Find the cursor's position in the body slice (or -1 if not in body).
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
	// If the cursor moved out of view, snap scroll.
	if cursorBodyIdx >= 0 {
		if cursorBodyIdx < m.scroll {
			m.scroll = cursorBodyIdx
		}
		if cursorBodyIdx >= m.scroll+vh {
			m.scroll = cursorBodyIdx - vh + 1
		}
	}
	end := m.scroll + vh
	if end > len(body) {
		end = len(body)
	}
	return body[m.scroll:end]
}

// bodyViewportH returns the number of body rows that fit. The body
// shares the listHeight with the fixed-overhead rows: header(1)+blank(1)
// at the top, footer separator+3 footer rows at the bottom. When
// creating a branch, the input row claims one more line.
func (m *SidebarModel) bodyViewportH() int {
	overhead := 2 + 4 // header block + footer block
	if m.creating {
		overhead++
	}
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
// top of the body.
func (m *SidebarModel) renderAllRow(selected bool) string {
	label := "All sessions"
	if selected {
		return lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ") +
			lipgloss.NewStyle().Foreground(textColor).Bold(true).Render(label)
	}
	if m.cursor == 0 {
		return lipgloss.NewStyle().Foreground(textColor).Render("  " + label)
	}
	return lipgloss.NewStyle().Foreground(dimColor).Render("  " + label)
}

// renderInputRow renders the inline new-branch input.
func (m *SidebarModel) renderInputRow() string {
	prefix := "  "
	if m.focused {
		prefix = lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
	}
	return prefix + m.input.View()
}

// rightCursorGlyph is the selection marker appended to the right edge
// of chevron-bearing rows (worktree + Older buckets). Keeps the chevron
// fixed at column 0 so the expand/collapse indicator stops shifting
// horizontally when the cursor lands on a row.
const rightCursorGlyph = "◀"

// rightCursorWidth is the visible width of " " + rightCursorGlyph.
const rightCursorWidth = 2

// renderWorktreeRow renders one worktree as a single line. Layout:
//
//	>label<  ☁           <repoLabel>  ◀     (collapsed)
//	<label>  ☁           <repoLabel>  ◀     (expanded)
//
// The brackets wrap the worktree name as the expand affordance — the
// arrows "point at" the label when collapsed (closed) and away from it
// when expanded (open). Cheaper visually than a triangle glyph and the
// affordance reads from either direction.
func (m *SidebarModel) renderWorktreeRow(n worktreeNode, idx int, selected bool, maxWidth int) string {
	if m.creating && m.cursor == idx {
		// While editing the branch input, suppress this row's selection
		// marker — the highlight follows where the user types.
		selected = false
	}

	leftBracket, rightBracket := ">", "<"
	if m.expanded[n.Key()] {
		leftBracket, rightBracket = "<", ">"
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

	// Reserve room for: two brackets (2) + owner glyph + repo tag + cursor.
	maxLabel := maxWidth - 2 - repoTagWidth - cursorReserve - lipgloss.Width(ownerGlyph)
	if maxLabel < 6 {
		maxLabel = 6
	}
	label := n.Label
	if len(label) > maxLabel {
		label = label[:maxLabel-1] + "…"
	}

	nameStyle := lipgloss.NewStyle().Foreground(textColor)
	if selected {
		nameStyle = nameStyle.Bold(true)
	} else if n.Archived == n.Total && n.Total > 0 {
		nameStyle = nameStyle.Foreground(mutedColor)
	} else if n.Active == 0 && n.Total > 0 {
		nameStyle = nameStyle.Foreground(dimColor)
	}

	line := nameStyle.Render(leftBracket+label+rightBracket) + ownerGlyph

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
// active is true. The rail is rendered in primaryColor and stays
// rendered regardless of pane focus — it marks "what's open," not
// "where the cursor is."
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
