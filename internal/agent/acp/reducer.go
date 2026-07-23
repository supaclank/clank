package acp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/acksell/clank/internal/agent"
	sdk "github.com/coder/acp-go-sdk"
)

// reducer translates the ACP session/update stream into clank's event
// vocabulary and maintains the in-memory transcript ([]agent.MessageData)
// — the only transcript clank keeps, per the no-persistence decision.
//
// Conventions it enforces (docs/chat-client-spec 03/04): a tool call and
// its result are TWO parts sharing one id (the ACP toolCallId), the
// result riding a role=user "carrier" message committed after the
// assistant message; streamed text/thinking arrive as is_delta=true
// chunks; unknown update variants drop without wedging the stream.
//
// Turn-scoped state closes at finishTurn — updates for a finished turn
// (the claude-adapter late-update class) find no open turn and drop with
// a log line. Session-scoped updates (title, mode) apply at any time.
type reducer struct {
	sid  string
	logf func(format string, args ...any)

	replaying bool
	messages  []agent.MessageData

	turnSeq int
	cur     *turnState

	title  string
	modeID string

	// replayUser accumulates consecutive user chunks during replay.
	replayUser *agent.MessageData
}

type turnState struct {
	seq            int
	assistantParts []agent.Part
	carrierParts   []agent.Part
	// openText indexes the assistant part receiving text/thinking chunks
	// (-1 = none). A tool call or a kind switch closes the run.
	openText     int
	openTextType agent.PartType
	partSeq      int
}

func newReducer(logf func(string, ...any)) *reducer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &reducer{logf: logf}
}

func (r *reducer) setSessionID(sid string) { r.sid = sid }

func (r *reducer) assistantMessageID(seq int) string { return fmt.Sprintf("%s:t%d", r.sid, seq) }
func (r *reducer) carrierMessageID(seq int) string   { return fmt.Sprintf("%s:t%d:carrier", r.sid, seq) }

// beginTurn opens turn-scoped state; the backend calls it when a prompt
// is dispatched (live) — replay opens turns implicitly on agent content.
func (r *reducer) beginTurn() {
	r.commitReplayUser()
	if r.cur != nil {
		r.logf("acp reducer: beginTurn with open turn t%d; committing", r.cur.seq)
		r.commitTurn()
	}
	r.turnSeq++
	r.cur = &turnState{seq: r.turnSeq, openText: -1}
}

// appendUserMessage records the prompt the backend just sent (the
// backend emits the corresponding EventMessage itself — it knows the
// text before any ACP traffic).
func (r *reducer) appendUserMessage(md agent.MessageData) {
	r.messages = append(r.messages, md)
}

// reduce translates one notification. Returned events are empty while
// replaying (session/load replays history; clients get it via Messages).
func (r *reducer) reduce(n sdk.SessionNotification) []agent.Event {
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		return r.onChunk(agent.PartText, u.AgentMessageChunk.Content)
	case u.AgentThoughtChunk != nil:
		return r.onChunk(agent.PartThinking, u.AgentThoughtChunk.Content)
	case u.UserMessageChunk != nil:
		return r.onUserChunk(u.UserMessageChunk.Content)
	case u.ToolCall != nil:
		return r.onToolCall(u.ToolCall)
	case u.ToolCallUpdate != nil:
		return r.onToolCallUpdate(u.ToolCallUpdate)
	case u.SessionInfoUpdate != nil:
		return r.onSessionInfo(u.SessionInfoUpdate)
	case u.CurrentModeUpdate != nil:
		r.modeID = string(u.CurrentModeUpdate.CurrentModeId)
		return nil
	case u.Plan != nil, u.PlanUpdate != nil, u.PlanRemoved != nil,
		u.AvailableCommandsUpdate != nil, u.ConfigOptionUpdate != nil:
		// Recognized but unmapped in v1 (plan approval is cut; commands/
		// config feed catalogs elsewhere).
		return nil
	default:
		b, _ := json.Marshal(u)
		r.logf("acp reducer: dropping unknown update variant: %s", truncate(string(b), 200))
		return nil
	}
}

// ensureTurn opens a turn implicitly — replay has no beginTurn calls, and
// a live update racing ahead of Send's bookkeeping must not be lost.
func (r *reducer) ensureTurn() *turnState {
	if r.cur == nil {
		r.beginTurn()
	}
	return r.cur
}

func (r *reducer) onChunk(pt agent.PartType, cb sdk.ContentBlock) []agent.Event {
	r.commitReplayUser()
	t := r.ensureTurn()
	text := contentBlockText(cb)
	if t.openText < 0 || t.openTextType != pt {
		t.partSeq++
		t.assistantParts = append(t.assistantParts, agent.Part{
			ID:     fmt.Sprintf("%s:t%d:p%d", r.sid, t.seq, t.partSeq),
			Type:   pt,
			Status: agent.PartRunning,
		})
		t.openText = len(t.assistantParts) - 1
		t.openTextType = pt
	}
	p := &t.assistantParts[t.openText]
	p.Text += text
	if r.replaying {
		return nil
	}
	delta := *p
	delta.Text = text
	return []agent.Event{{
		Type: agent.EventPartUpdate,
		Data: agent.PartUpdateData{MessageID: r.assistantMessageID(t.seq), Part: delta, IsDelta: true},
	}}
}

// onUserChunk: live turns already emitted the user message at Send, so
// live chunks drop; replay builds user messages from them, closing any
// open assistant turn first (a user message bounds the previous turn).
func (r *reducer) onUserChunk(cb sdk.ContentBlock) []agent.Event {
	if !r.replaying {
		r.logf("acp reducer: dropping live user_message_chunk (prompt already recorded)")
		return nil
	}
	if r.cur != nil {
		r.commitTurn()
	}
	text := contentBlockText(cb)
	if r.replayUser == nil {
		r.replayUser = &agent.MessageData{Role: "user"}
	}
	r.replayUser.Content += text
	return nil
}

func (r *reducer) commitReplayUser() {
	if r.replayUser == nil {
		return
	}
	md := *r.replayUser
	md.ID = fmt.Sprintf("%s:r%d:user", r.sid, len(r.messages))
	r.messages = append(r.messages, md)
	r.replayUser = nil
}

func (r *reducer) onToolCall(tc *sdk.SessionUpdateToolCall) []agent.Event {
	r.commitReplayUser()
	t := r.ensureTurn()
	t.openText = -1
	part := agent.Part{
		ID:     string(tc.ToolCallId),
		Type:   agent.PartToolCall,
		Tool:   toolName(tc.Title, tc.Meta),
		Status: mapToolStatus(tc.Status, agent.PartRunning),
		Input:  asMap(tc.RawInput),
	}
	t.assistantParts = append(t.assistantParts, part)
	if r.replaying {
		return nil
	}
	return []agent.Event{{
		Type: agent.EventPartUpdate,
		Data: agent.PartUpdateData{MessageID: r.assistantMessageID(t.seq), Part: part, IsDelta: false},
	}}
}

func (r *reducer) onToolCallUpdate(up *sdk.SessionToolCallUpdate) []agent.Event {
	t := r.cur
	if t == nil {
		r.logf("acp reducer: dropping tool_call_update %s outside any turn (late update)", up.ToolCallId)
		return nil
	}
	idx := -1
	for i := range t.assistantParts {
		if t.assistantParts[i].Type == agent.PartToolCall && t.assistantParts[i].ID == string(up.ToolCallId) {
			idx = i
			break
		}
	}
	if idx < 0 {
		r.logf("acp reducer: dropping tool_call_update for unknown call %s (late update)", up.ToolCallId)
		return nil
	}
	call := &t.assistantParts[idx]
	if up.Title != nil && call.Tool == "" {
		call.Tool = *up.Title
	}
	if up.RawInput != nil {
		call.Input = asMap(up.RawInput)
	}
	terminal := false
	if up.Status != nil {
		switch *up.Status {
		case sdk.ToolCallStatusPending:
			// Status is monotonic client-side (DATA-021); never regress.
		case sdk.ToolCallStatusInProgress:
			if call.Status == agent.PartPending {
				call.Status = agent.PartRunning
			}
		case sdk.ToolCallStatusCompleted:
			call.Status = agent.PartCompleted
			terminal = true
		case sdk.ToolCallStatusFailed:
			call.Status = agent.PartFailed
			terminal = true
		}
	}
	var events []agent.Event
	if !r.replaying {
		events = append(events, agent.Event{
			Type: agent.EventPartUpdate,
			Data: agent.PartUpdateData{MessageID: r.assistantMessageID(t.seq), Part: *call, IsDelta: false},
		})
	}
	if terminal {
		result := agent.Part{
			ID:     string(up.ToolCallId),
			Type:   agent.PartToolResult,
			Output: flattenToolOutput(up),
			Status: call.Status,
		}
		t.carrierParts = append(t.carrierParts, result)
		if !r.replaying {
			events = append(events, agent.Event{
				Type: agent.EventPartUpdate,
				Data: agent.PartUpdateData{MessageID: r.carrierMessageID(t.seq), Part: result, IsDelta: false},
			})
		}
	}
	return events
}

func (r *reducer) onSessionInfo(si *sdk.SessionSessionInfoUpdate) []agent.Event {
	if si.Title == nil || *si.Title == "" || *si.Title == r.title {
		return nil
	}
	r.title = *si.Title
	// Titles are session-scoped: emitted even between turns, and during
	// replay (the host persists them via metadata, not the transcript).
	return []agent.Event{{Type: agent.EventTitleChange, Data: agent.TitleChangeData{Title: r.title}}}
}

// commitTurn moves the open turn into the committed transcript and
// returns the committed-message events (assistant, then the tool-result
// carrier) — empty while replaying.
func (r *reducer) commitTurn() []agent.Event {
	t := r.cur
	r.cur = nil
	if t == nil {
		return nil
	}
	var events []agent.Event
	if len(t.assistantParts) > 0 {
		var text strings.Builder
		for _, p := range t.assistantParts {
			if p.Type == agent.PartText {
				text.WriteString(p.Text)
			}
		}
		for i := range t.assistantParts {
			if t.assistantParts[i].Status == agent.PartRunning || t.assistantParts[i].Status == agent.PartPending {
				t.assistantParts[i].Status = agent.PartCompleted
			}
		}
		md := agent.MessageData{
			ID:      r.assistantMessageID(t.seq),
			Role:    "assistant",
			Content: text.String(),
			Parts:   t.assistantParts,
		}
		r.messages = append(r.messages, md)
		if !r.replaying {
			events = append(events, agent.Event{Type: agent.EventMessage, Data: md})
		}
	}
	if len(t.carrierParts) > 0 {
		md := agent.MessageData{
			ID:    r.carrierMessageID(t.seq),
			Role:  "user",
			Parts: t.carrierParts,
		}
		r.messages = append(r.messages, md)
		if !r.replaying {
			events = append(events, agent.Event{Type: agent.EventMessage, Data: md})
		}
	}
	return events
}

// finishTurn is commitTurn for the live path — called by the turn runner
// after the prompt response (and the late-update drain) so committed
// messages land exactly once per turn.
func (r *reducer) finishTurn() []agent.Event { return r.commitTurn() }

// finishReplay closes replay state after session/load returns.
func (r *reducer) finishReplay() {
	r.commitReplayUser()
	if r.cur != nil {
		r.commitTurn()
	}
	r.replaying = false
}

// messageCount reports the transcript length so a caller can snapshot
// it before a replay attempt and roll back on failure.
func (r *reducer) messageCount() int { return len(r.messages) }

// rollbackReplay discards messages/turn state accumulated by a failed
// session/load, so a retried Open on the same Backend doesn't duplicate
// history when the adapter replays it again from the start.
func (r *reducer) rollbackReplay(preLoadCount int) {
	r.messages = r.messages[:preLoadCount]
	r.cur = nil
	r.replayUser = nil
	r.replaying = false
}

// snapshot deep-copies the transcript, including the in-flight turn as a
// partial assistant message (clients reconcile via the monotonic rule).
func (r *reducer) snapshot() []agent.MessageData {
	out := make([]agent.MessageData, 0, len(r.messages)+2)
	for _, m := range r.messages {
		m.Parts = clonePartsDeep(m.Parts)
		out = append(out, m)
	}
	if t := r.cur; t != nil && len(t.assistantParts) > 0 {
		out = append(out, agent.MessageData{
			ID:    r.assistantMessageID(t.seq),
			Role:  "assistant",
			Parts: clonePartsDeep(t.assistantParts),
		})
		if len(t.carrierParts) > 0 {
			out = append(out, agent.MessageData{
				ID:    r.carrierMessageID(t.seq),
				Role:  "user",
				Parts: clonePartsDeep(t.carrierParts),
			})
		}
	}
	return out
}

// clonePartsDeep copies parts plus their Input map and Question pointer so
// a caller mutating the returned snapshot can't corrupt live reducer state.
func clonePartsDeep(parts []agent.Part) []agent.Part {
	out := make([]agent.Part, len(parts))
	for i, p := range parts {
		if p.Input != nil {
			input := make(map[string]any, len(p.Input))
			for k, v := range p.Input {
				input[k] = v
			}
			p.Input = input
		}
		if p.Question != nil {
			q := *p.Question
			p.Question = &q
		}
		out[i] = p
	}
	return out
}

// lastMessageID supports tip-only fork checks.
func (r *reducer) lastMessageID() string {
	if len(r.messages) == 0 {
		return ""
	}
	return r.messages[len(r.messages)-1].ID
}

// --- helpers ---

func contentBlockText(cb sdk.ContentBlock) string {
	if cb.Text != nil {
		return cb.Text.Text
	}
	b, _ := json.Marshal(cb)
	return string(b)
}

func toolName(title string, meta map[string]any) string {
	// claude-agent-acp carries the raw tool name in
	// _meta.claudeCode.toolName; prefer it over the display title.
	if cc, ok := meta["claudeCode"].(map[string]any); ok {
		if n, ok := cc["toolName"].(string); ok && n != "" {
			return n
		}
	}
	return title
}

func mapToolStatus(s sdk.ToolCallStatus, fallback agent.PartStatus) agent.PartStatus {
	switch s {
	case sdk.ToolCallStatusPending:
		return agent.PartPending
	case sdk.ToolCallStatusInProgress:
		return agent.PartRunning
	case sdk.ToolCallStatusCompleted:
		return agent.PartCompleted
	case sdk.ToolCallStatusFailed:
		return agent.PartFailed
	default:
		return fallback
	}
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// flattenToolOutput renders a tool result to text: content blocks joined,
// else rawOutput (string or JSON).
func flattenToolOutput(up *sdk.SessionToolCallUpdate) string {
	var parts []string
	for _, c := range up.Content {
		switch {
		case c.Content != nil:
			parts = append(parts, contentBlockText(c.Content.Content))
		case c.Diff != nil:
			b, _ := json.Marshal(c.Diff)
			parts = append(parts, string(b))
		case c.Terminal != nil:
			parts = append(parts, "[terminal output]")
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	switch o := up.RawOutput.(type) {
	case nil:
		return ""
	case string:
		return o
	default:
		b, _ := json.Marshal(o)
		return string(b)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
