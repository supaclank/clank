package tui

import (
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
)

// sessionTitle returns the canonical title text for a session, on a
// single line and with no styling applied. Precedence: Title (AI-generated
// by the backend) → Prompt (user's initial message) → truncated ID as
// last-resort fallback. Title and Prompt are flattened because a prompt
// (and thus a derived title) can carry newlines; a multi-row title breaks
// callers that budget a fixed number of rows (e.g. the sidebar's two-line
// session rows). Callers truncate to fit their layout and apply
// state-dependent colour.
func sessionTitle(s agent.SessionInfo) string {
	if s.Title != "" {
		return singleLine(s.Title)
	}
	if s.Prompt != "" {
		return singleLine(s.Prompt)
	}
	return truncateStr(s.ID, 8)
}

// sessionTitleIsFallback reports whether sessionTitle had to fall back to
// the truncated ID. Used by callers that dim the row when there's no real
// title to show.
func sessionTitleIsFallback(s agent.SessionInfo) bool {
	return s.Title == "" && s.Prompt == ""
}

// sessionMarker returns the styled unread/follow-up glyph for a session,
// or a single space when the session has neither marker. Order of
// precedence matches the inbox: follow-up (`!`) wins over unread (`*`).
func sessionMarker(s agent.SessionInfo) string {
	if s.FollowUp {
		return lipgloss.NewStyle().Foreground(warningColor).Bold(true).Render("!")
	}
	if s.Unread() {
		return lipgloss.NewStyle().Foreground(dangerColor).Bold(true).Render("*")
	}
	return " "
}
