package tui

// sessionview_question.go — interactive question prompts.
//
// Backends tag a question tool call's Part with the normalized prompt
// (agent.Part.Question — Claude AskUserQuestion, OpenCode question), on both
// streamed part events and Messages() history. The TUI therefore has no
// question-specific wire state: the active prompt is DERIVED from the
// transcript — a tagged tool part is answerable iff it is the conversation's
// last content entry (nothing after it superseded it) and wasn't answered
// here. Reopening a session restores the card from the ordinary history
// refetch for free.
//
// Answers go back via the questions reply endpoint using the tag's
// request_id; the backend owns the provider transport. The generic permission
// prompt that gates the tool in Claude's default/plan modes is suppressed
// (matched by request id / part id) — the card supersedes it.
//
// Key model while a question prompt is active:
//
//	↑/k ↓/j   move the option cursor
//	1-9       toggle that option (single-select: pick it and advance)
//	space/x   toggle the option under the cursor
//	o         edit the free-text "Other" answer (when the question allows it)
//	enter     next question; on the last one, submit all answers
//	backspace previous question
//	esc       dismiss the whole prompt (best-effort reject; always unlocks)

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
)

// questionReplyResultMsg is sent after the daemon accepts or rejects a
// question reply.
type questionReplyResultMsg struct {
	requestID string
	questions []agent.Question
	answers   []agent.QuestionAnswer
	reject    bool
	err       error
}

// newQuestionTextInput builds the single-line input used for free-text
// ("Other") answers and plan revision notes.
func newQuestionTextInput(placeholder string) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.CharLimit = 2000
	ti.Prompt = "› "
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(primaryColor)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)
	return ti
}

// promptActive reports whether a blocking prompt (permission or question) is
// on screen or a reply is in flight — states in which normal navigation and
// composing are locked out.
func (m *SessionViewModel) promptActive() bool {
	return len(m.pendingPerms) > 0 || m.replyingPermID != "" ||
		m.activeQuestionPart() != nil || m.replyingQuestionID != ""
}

// activeQuestionPart returns the tool part of the question prompt currently
// awaiting an answer, or nil. A tagged part is answerable iff it is the last
// content entry in the transcript — anything after it (agent text, another
// tool, a user message) means the conversation moved on — and it wasn't
// answered or dismissed in this view. Status "error" (deny/abort fallout)
// also retires it.
func (m *SessionViewModel) activeQuestionPart() *agent.Part {
	for i := len(m.entries) - 1; i >= 0; i-- {
		e := &m.entries[i]
		// Decorations don't count as the conversation moving on.
		if e.kind == entryStatus || e.kind == entryPermResult {
			continue
		}
		if e.kind != entryTool || e.toolPart == nil || e.toolPart.Question == nil {
			return nil
		}
		p := e.toolPart
		if p.Status == agent.PartFailed || m.answeredQuestions[p.Question.RequestID] {
			return nil
		}
		return p
	}
	return nil
}

// syncQuestionUI resets the per-prompt selection state when the active
// question changes (or clears). Called before key handling and rendering so
// derived state and UI state can't diverge.
func (m *SessionViewModel) syncQuestionUI() *agent.Part {
	p := m.activeQuestionPart()
	if p == nil {
		m.activeQuestionID = ""
		return nil
	}
	if m.activeQuestionID != p.ID {
		m.activeQuestionID = p.ID
		n := len(p.Question.Questions)
		m.questionSel = make([]map[int]bool, n)
		for i := range m.questionSel {
			m.questionSel[i] = make(map[int]bool)
		}
		m.questionCustom = make([]string, n)
		m.questionIdx = 0
		m.questionCursor = 0
		m.questionTyping = false
		// The prompt needs answers — release the textarea so keys reach the
		// question handler instead of being typed into the composer.
		m.deactivateInput()
	}
	return p
}

// questionSuppressesPermission reports whether a permission prompt is
// superseded by the question card for the same request (Claude gated mode
// emits both; the tagged part always precedes the permission).
func (m *SessionViewModel) questionSuppressesPermission(perm agent.PermissionData) bool {
	p := m.activeQuestionPart()
	if p == nil {
		return false
	}
	return p.Question.RequestID == perm.RequestID || (perm.ToolUseID != "" && perm.ToolUseID == p.ID)
}

// dropPermission removes a pending permission prompt by request id without
// replying to it (its question card was answered; the backend resolves the
// parked prompt through the question reply).
func (m *SessionViewModel) dropPermission(requestID string) {
	filtered := m.pendingPerms[:0]
	for _, p := range m.pendingPerms {
		if p.RequestID != requestID {
			filtered = append(filtered, p)
		}
	}
	m.pendingPerms = filtered
}

// handleQuestionKey processes a key press while a question prompt is active.
// Returns handled=false when no prompt is active or for keys the caller owns
// (ctrl+c).
func (m *SessionViewModel) handleQuestionKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return false, nil
	}
	// A reply in flight swallows keys until the result lands.
	if m.replyingQuestionID != "" {
		return true, nil
	}
	p := m.syncQuestionUI()
	if p == nil {
		return false, nil
	}

	prompt := p.Question
	q := prompt.Questions[m.questionIdx]
	otherRow := len(q.Options) // cursor position of the "Other" row, when shown

	if m.questionTyping {
		switch msg.String() {
		case "esc":
			m.questionTyping = false
			m.questionInput.Blur()
			return true, nil
		case "enter":
			m.questionCustom[m.questionIdx] = strings.TrimSpace(m.questionInput.Value())
			m.questionTyping = false
			m.questionInput.Blur()
			return true, nil
		default:
			var cmd tea.Cmd
			m.questionInput, cmd = m.questionInput.Update(tea.Msg(msg))
			return true, cmd
		}
	}

	maxCursor := len(q.Options) - 1
	if q.AllowCustom {
		maxCursor = otherRow
	}

	switch msg.String() {
	case "up", "k":
		if m.questionCursor > 0 {
			m.questionCursor--
		}
		return true, nil
	case "down", "j":
		if m.questionCursor < maxCursor {
			m.questionCursor++
		}
		return true, nil
	case "space", "x", " ":
		if m.questionCursor == otherRow && q.AllowCustom {
			m.startQuestionTyping()
			return true, nil
		}
		m.toggleQuestionOption(q, m.questionCursor)
		return true, nil
	case "o":
		if q.AllowCustom {
			m.startQuestionTyping()
		}
		return true, nil
	case "enter":
		// On a single-select question with nothing chosen yet, enter picks the
		// highlighted option before advancing.
		if !q.MultiSelect && len(m.questionSel[m.questionIdx]) == 0 &&
			m.questionCursor < len(q.Options) {
			m.questionSel[m.questionIdx][m.questionCursor] = true
		}
		if m.questionIdx < len(prompt.Questions)-1 {
			m.questionIdx++
			m.questionCursor = 0
			return true, nil
		}
		return true, m.submitQuestionAnswers(prompt)
	case "backspace":
		if m.questionIdx > 0 {
			m.questionIdx--
			m.questionCursor = 0
		}
		return true, nil
	case "esc":
		m.replyingQuestionID = prompt.RequestID
		return true, m.replyQuestion(prompt, nil, true)
	}

	// Digit keys toggle the corresponding option.
	if s := msg.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		idx := int(s[0] - '1')
		if idx < len(q.Options) {
			m.toggleQuestionOption(q, idx)
			m.questionCursor = idx
			// Single-select: picking by digit also advances to the next
			// question (but never auto-submits on the last one).
			if !q.MultiSelect && m.questionSel[m.questionIdx][idx] &&
				m.questionIdx < len(prompt.Questions)-1 {
				m.questionIdx++
				m.questionCursor = 0
			}
		}
		return true, nil
	}

	// Lock out everything else while the prompt is up.
	return true, nil
}

func (m *SessionViewModel) startQuestionTyping() {
	p := m.activeQuestionPart()
	if p == nil {
		return
	}
	m.questionTyping = true
	m.questionCursor = len(p.Question.Questions[m.questionIdx].Options)
	m.questionInput = newQuestionTextInput("Type your answer...")
	m.questionInput.SetValue(m.questionCustom[m.questionIdx])
	m.questionInput.Focus()
}

func (m *SessionViewModel) toggleQuestionOption(q agent.Question, idx int) {
	if idx < 0 || idx >= len(q.Options) {
		return
	}
	sel := m.questionSel[m.questionIdx]
	if q.MultiSelect {
		if sel[idx] {
			delete(sel, idx)
		} else {
			sel[idx] = true
		}
		return
	}
	// Single-select: picking again clears; picking another replaces.
	if sel[idx] {
		delete(sel, idx)
		return
	}
	for k := range sel {
		delete(sel, k)
	}
	sel[idx] = true
}

// submitQuestionAnswers converts the selection state into structured answers
// and dispatches the reply.
func (m *SessionViewModel) submitQuestionAnswers(prompt *agent.QuestionPrompt) tea.Cmd {
	answers := make([]agent.QuestionAnswer, len(prompt.Questions))
	for i, q := range prompt.Questions {
		var selected []string
		for idx := range q.Options {
			if m.questionSel[i][idx] {
				selected = append(selected, q.Options[idx].Label)
			}
		}
		answers[i] = agent.QuestionAnswer{Selected: selected, Custom: m.questionCustom[i]}
	}
	m.replyingQuestionID = prompt.RequestID
	return m.replyQuestion(prompt, answers, false)
}

func (m *SessionViewModel) replyQuestion(prompt *agent.QuestionPrompt, answers []agent.QuestionAnswer, reject bool) tea.Cmd {
	client := m.client
	sessionID := m.sessionID
	requestID := prompt.RequestID
	questions := prompt.Questions
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := client.Session(sessionID).ReplyQuestion(ctx, requestID, answers, reject)
		return questionReplyResultMsg{requestID: requestID, questions: questions, answers: answers, reject: reject, err: err}
	}
}

// handleQuestionReplyResult applies a questionReplyResultMsg to the model.
func (m *SessionViewModel) handleQuestionReplyResult(msg questionReplyResultMsg) {
	m.replyingQuestionID = ""
	if msg.err != nil {
		m.err = msg.err
		// A failed answer keeps the card for retry; a failed dismissal still
		// dismisses locally so the UI can never lock into a dead prompt.
		if !msg.reject {
			return
		}
	}
	m.answeredQuestions[msg.requestID] = true
	// The gating permission (Claude default/plan mode) was resolved through
	// the question reply; drop the suppressed local copy if one queued.
	m.dropPermission(msg.requestID)
	m.entries = append(m.entries, displayEntry{
		kind:        entryPermResult,
		content:     summarizeQuestionReply(msg.questions, msg.answers, msg.reject),
		permGranted: !msg.reject,
	})
	if m.follow {
		m.scrollToBottom()
	}
}

// summarizeQuestionReply renders the one-line transcript record of a question
// reply, e.g. `Answered — Auth: JWT · Storage: (delegated)`.
func summarizeQuestionReply(questions []agent.Question, answers []agent.QuestionAnswer, reject bool) string {
	if reject {
		return "Dismissed questions"
	}
	parts := make([]string, 0, len(questions))
	for i, q := range questions {
		var a agent.QuestionAnswer
		if i < len(answers) {
			a = answers[i]
		}
		answer := strings.Join(a.Selected, ", ")
		if custom := strings.TrimSpace(a.Custom); custom != "" {
			if answer != "" {
				answer += " (Other: " + custom + ")"
			} else {
				answer = custom
			}
		}
		if answer == "" {
			answer = "(delegated)"
		}
		parts = append(parts, q.Header+": "+answer)
	}
	return "Answered — " + strings.Join(parts, " · ")
}

// planTextFor returns the plan text for an ExitPlanMode permission prompt by
// locating the tool part the prompt gates (matched by tool_use id, falling
// back to the most recent ExitPlanMode part with a plan).
func (m *SessionViewModel) planTextFor(perm agent.PermissionData) string {
	var fallback string
	for i := len(m.entries) - 1; i >= 0; i-- {
		e := &m.entries[i]
		if e.kind != entryTool || e.toolPart == nil || e.toolPart.Tool != agent.ClaudeToolExitPlanMode {
			continue
		}
		plan, _ := e.toolPart.Input["plan"].(string)
		if plan == "" {
			continue
		}
		if e.toolPart.ID == perm.ToolUseID || perm.ToolUseID == "" {
			return plan
		}
		if fallback == "" {
			fallback = plan
		}
	}
	return fallback
}

// renderQuestionCard renders the interactive prompt card appended below the
// transcript, mirroring the permission card's framing. Callers guarantee an
// active question exists (syncQuestionUI returned non-nil).
func (m *SessionViewModel) renderQuestionCard(p *agent.Part) []string {
	prompt := p.Question
	q := prompt.Questions[m.questionIdx]

	maxWidth := m.width
	if maxWidth < 20 {
		maxWidth = 20
	}
	innerWidth := maxWidth - 4 // border (2) + padding (2)
	if innerWidth < 16 {
		innerWidth = 16
	}

	headStyle := lipgloss.NewStyle().Foreground(primaryColor).Bold(true)
	dim := lipgloss.NewStyle().Foreground(dimColor)
	txt := lipgloss.NewStyle().Foreground(textColor)

	title := "? " + q.Header
	if len(prompt.Questions) > 1 {
		title += fmt.Sprintf("  (%d/%d)", m.questionIdx+1, len(prompt.Questions))
	}

	var content []string
	content = append(content, headStyle.Render(title))
	for _, l := range strings.Split(wrapText(q.Text, innerWidth), "\n") {
		content = append(content, txt.Render(l))
	}
	if q.MultiSelect {
		content = append(content, dim.Render("select all that apply"))
	}
	content = append(content, "")

	sel := m.questionSel[m.questionIdx]
	for idx, opt := range q.Options {
		content = append(content, m.renderQuestionOption(idx, opt, sel[idx], q.MultiSelect, innerWidth)...)
	}
	if q.AllowCustom {
		cursor := "  "
		if m.questionCursor == len(q.Options) {
			cursor = "> "
		}
		line := cursor + "   Other: "
		if m.questionTyping {
			content = append(content, txt.Render(line)+m.questionInput.View())
		} else if m.questionCustom[m.questionIdx] != "" {
			content = append(content, txt.Render(line+m.questionCustom[m.questionIdx]))
		} else {
			content = append(content, dim.Render(line+"(press o to type)"))
		}
	}

	content = append(content, "")
	if m.replyingQuestionID != "" {
		content = append(content, dim.Render("Sending..."))
	} else if m.questionTyping {
		content = append(content, dim.Render("enter: save · esc: cancel"))
	} else {
		hint := "↑/↓ move · space/1-9 select · enter next · esc dismiss"
		if m.questionIdx == len(prompt.Questions)-1 {
			hint = "↑/↓ move · space/1-9 select · enter submit · esc dismiss"
		}
		if m.questionIdx > 0 {
			hint += " · backspace back"
		}
		content = append(content, dim.Render(hint))
	}

	bordered := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(0, 1).
		Render(strings.Join(content, "\n"))
	return strings.Split(bordered, "\n")
}

func (m *SessionViewModel) renderQuestionOption(idx int, opt agent.QuestionOption, selected, multi bool, innerWidth int) []string {
	dim := lipgloss.NewStyle().Foreground(dimColor)
	txt := lipgloss.NewStyle().Foreground(textColor)
	selStyle := lipgloss.NewStyle().Foreground(successColor)

	cursor := "  "
	if !m.questionTyping && m.questionCursor == idx {
		cursor = "> "
	}
	box := "( )"
	if multi {
		box = "[ ]"
	}
	if selected {
		if multi {
			box = "[x]"
		} else {
			box = "(•)"
		}
	}
	label := fmt.Sprintf("%s%d. %s %s", cursor, idx+1, box, opt.Label)
	style := txt
	if selected {
		style = selStyle
	}
	lines := []string{style.Render(label)}
	if opt.Description != "" {
		descWidth := innerWidth - 9
		if descWidth < 10 {
			descWidth = 10
		}
		for _, l := range strings.Split(wrapText(opt.Description, descWidth), "\n") {
			lines = append(lines, dim.Render("         "+l))
		}
	}
	return lines
}
