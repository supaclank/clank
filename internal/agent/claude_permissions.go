package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// permissionDecision is the user's answer to a parked permission prompt.
// denyMessage is the reason forwarded to the model when allow is false; it is
// ignored when allow is true.
type permissionDecision struct {
	allow       bool
	denyMessage string
}

// parkedPermission is a permission prompt awaiting RespondPermission: the
// decision channel the parked handleCanUseTool blocks on, plus the prompt
// snapshot that PendingPermissions and the synthetic Messages part serve to
// clients that (re)join mid-park.
type parkedPermission struct {
	seq      uint64
	decision chan permissionDecision
	data     PermissionData
	// input is the gated tool's arguments, kept so a rejoining client can
	// render the tool card (plan text, question options) whose tool_use block
	// the CLI hasn't flushed to the transcript yet.
	input map[string]any
	// questions is the parsed AskUserQuestion prompt (nil for other tools).
	questions []Question
}

// claudeQuestion is a pending AskUserQuestion prompt awaiting RespondQuestion.
type claudeQuestion struct {
	toolUseID string
	questions []Question
	// viaPerm marks a prompt parked on handleCanUseTool (gated modes): the
	// formatted answers ride the permission deny. When false the tool auto-ran
	// (bypassPermissions) and answers go back as a follow-up user message.
	viaPerm bool
}

// handleCanUseTool is the SDK CanUseTool callback. The SDK invokes it
// synchronously on its control-protocol read goroutine whenever the CLI wants
// to use a tool the current permission mode doesn't auto-approve. It bridges
// that synchronous call to clank's asynchronous permission UI: it emits an
// EventPermission (which reaches the TUI through the same path OpenCode uses)
// and blocks until RespondPermission delivers the user's decision.
//
// Blocking the read goroutine here is safe: the CLI is itself paused awaiting
// the decision, so no other messages are due until we answer.
func (b *ClaudeCodeBackend) handleCanUseTool(ctx context.Context, tool string, input map[string]any, _ claudecode.ToolPermissionContext) (claudecode.PermissionResult, error) {
	decision := make(chan permissionDecision, 1)

	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return claudecode.NewPermissionResultDeny("session stopped"), nil
	}
	b.permSeq++
	id := fmt.Sprintf("perm-%d", b.permSeq)
	toolUseID := b.lastToolUseID[tool]

	// Normalize AskUserQuestion by tagging its tool part with the structured
	// prompt (see Part.Question) instead of a side-channel event: the part is
	// what clients already reconcile across both the live stream and history
	// refetch, so the prompt survives reopening the session. The permission
	// event still follows (same request id) for clients that predate the tag;
	// tag-aware clients suppress it by matching request_id / tool_use_id.
	var questions []Question
	if tool == ClaudeToolAskUserQuestion {
		if questions = parseClaudeQuestions(input); questions != nil {
			b.pendingQuestions[id] = claudeQuestion{toolUseID: toolUseID, questions: questions, viaPerm: true}
		}
	}
	data := PermissionData{
		RequestID:   id,
		Tool:        tool,
		Description: describeToolCall(tool, input),
		ToolUseID:   toolUseID,
	}
	b.pendingPerms[id] = &parkedPermission{
		seq:       b.permSeq,
		decision:  decision,
		data:      data,
		input:     input,
		questions: questions,
	}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pendingPerms, id)
		delete(b.pendingQuestions, id)
		b.mu.Unlock()
	}()

	// The CLI asks for permission just before the content_block_stop that would
	// carry the tool's input, and this callback parks the single stdout reader —
	// so that stop (and the input) never reaches clients until the prompt is
	// answered. Stop-and-wait tools (ExitPlanMode, AskUserQuestion) render their
	// answer UI *from* that input, so without it the client is stuck on a spinner
	// it can't act on. Emit the input now, from the map the CLI just handed us, so
	// the card can render while the prompt is pending. MessageID is left empty
	// (currentMsgID is owned by receiveLoop; reading it here would race), and
	// clients attach tool parts by part id.
	if toolUseID != "" {
		part := Part{
			ID:     toolUseID,
			Type:   PartToolCall,
			Tool:   tool,
			Status: PartRunning,
			Input:  input,
		}
		if questions != nil {
			part.Question = &QuestionPrompt{RequestID: id, Questions: questions}
		}
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data:      PartUpdateData{Part: part},
		})
	}

	b.emit(Event{
		Type:      EventPermission,
		Timestamp: time.Now(),
		Data:      data,
	})

	select {
	case d := <-decision:
		if d.allow {
			// Approving ExitPlanMode makes the CLI auto-exit plan mode (it
			// transitions the session to its post-plan mode on its own). clank
			// can't see that new mode, so its currentPermMode would go stale at
			// "plan" — and Send's "skip SetPermissionMode if unchanged" check
			// would then wrongly skip re-asserting plan on the next message
			// (running it in the CLI's default mode instead). Reset the tracked
			// mode to unknown so the next Send always re-asserts the user's
			// chosen mode. The re-assert always succeeds: the session was
			// launched with --dangerously-skip-permissions (see Open).
			if tool == ClaudeToolExitPlanMode {
				b.mu.Lock()
				b.currentPermMode = ""
				b.mu.Unlock()
			}
			// The CLI validates the permission response against a discriminated
			// union whose allow branch requires updatedInput to be a record; a
			// bare {behavior:"allow"} matches neither allow nor deny and the CLI
			// fails the tool with a ZodError. Echo the unmodified input back as
			// updatedInput (the SDK guarantees it is non-nil) so the schema is
			// satisfied without changing what the tool runs with.
			//
			// TODO: drop the explicit UpdatedInput line once the SDK fills it at
			// the boundary — https://github.com/severity1/claude-agent-sdk-go/pull/100
			result := claudecode.NewPermissionResultAllow()
			result.UpdatedInput = input
			return result, nil
		}
		// The deny branch of the same union requires a string message, so never
		// send an empty one.
		msg := d.denyMessage
		if msg == "" {
			msg = "denied by user"
		}
		return claudecode.NewPermissionResultDeny(msg), nil
	case <-ctx.Done():
		return claudecode.NewPermissionResultDeny("cancelled"), nil
	case <-b.ctx.Done():
		return claudecode.NewPermissionResultDeny("session stopped"), nil
	}
}

// RespondPermission delivers the user's decision to a parked handleCanUseTool
// call. allow=true permits the tool once; allow=false denies it with denyMessage
// as the reason shown to the model (empty falls back to a default). It is
// idempotent and never blocks (the decision channel is buffered), and returns
// an error for an unknown ID so callers fail fast on a stale prompt.
func (b *ClaudeCodeBackend) RespondPermission(_ context.Context, permissionID string, allow bool, denyMessage string) error {
	b.mu.Lock()
	parked, ok := b.pendingPerms[permissionID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending permission %q", permissionID)
	}
	select {
	case parked.decision <- permissionDecision{allow: allow, denyMessage: denyMessage}:
	default:
	}
	return nil
}

// PendingPermissions returns the parked permission prompts, oldest first.
// In-memory only: a parked prompt lives exactly as long as the blocked
// handleCanUseTool call, so there is nothing to serve across a restart.
func (b *ClaudeCodeBackend) PendingPermissions(_ context.Context) ([]PermissionData, error) {
	parked := b.parkedPermissions()
	perms := make([]PermissionData, len(parked))
	for i, p := range parked {
		perms[i] = p.data
	}
	return perms, nil
}

// parkedPermissions snapshots the pending permission queue, oldest first.
func (b *ClaudeCodeBackend) parkedPermissions() []*parkedPermission {
	b.mu.Lock()
	defer b.mu.Unlock()
	parked := make([]*parkedPermission, 0, len(b.pendingPerms))
	for _, p := range b.pendingPerms {
		parked = append(parked, p)
	}
	sort.Slice(parked, func(i, j int) bool { return parked[i].seq < parked[j].seq })
	return parked
}

// pendingPermissionMessages synthesizes an in-flight assistant message for
// each parked permission whose tool_use block isn't in the transcript yet:
// the CLI requests permission before flushing the block, so a client that
// (re)joins mid-park would otherwise have no part to render the gated tool
// from — most visibly the AskUserQuestion card and the ExitPlanMode plan.
// The part mirrors the one handleCanUseTool emitted live (same part id, same
// question tag), so answering works identically from restored state.
func (b *ClaudeCodeBackend) pendingPermissionMessages(transcript []MessageData) []MessageData {
	flushed := make(map[string]bool)
	for _, m := range transcript {
		for _, p := range m.Parts {
			if p.Type == PartToolCall {
				flushed[p.ID] = true
			}
		}
	}

	var msgs []MessageData
	for _, parked := range b.parkedPermissions() {
		if parked.data.ToolUseID == "" || flushed[parked.data.ToolUseID] {
			continue
		}
		part := Part{
			ID:     parked.data.ToolUseID,
			Type:   PartToolCall,
			Tool:   parked.data.Tool,
			Status: PartRunning,
			Input:  parked.input,
		}
		if len(parked.questions) > 0 {
			// RequestID is the permission id (not the "q-" bypass form) so the
			// reply routes through the parked prompt (RespondQuestion viaPerm).
			part.Question = &QuestionPrompt{RequestID: parked.data.RequestID, Questions: parked.questions}
		}
		msgs = append(msgs, MessageData{
			ID:    "pending-" + parked.data.RequestID,
			Role:  "assistant",
			Parts: []Part{part},
		})
	}
	return msgs
}

// RespondQuestion delivers structured answers to a question prompt.
// For a prompt parked on handleCanUseTool (gated modes) the formatted answers
// ride the permission deny — allow would run the tool's interactive picker
// headless and re-deadlock, so deny-with-message is the only transport that
// reaches the model. For a prompt whose tool auto-ran (bypassPermissions) the
// answers are dispatched as a follow-up user message, mirroring the mobile
// client's fallback. A "q-<tool_use_id>" request not in the in-memory map
// (e.g. after a daemon restart) is recovered from the transcript, so answers
// to a question restored from history always work.
func (b *ClaudeCodeBackend) RespondQuestion(ctx context.Context, requestID string, answers []QuestionAnswer, reject bool) error {
	b.mu.Lock()
	q, ok := b.pendingQuestions[requestID]
	b.mu.Unlock()
	if !ok {
		var err error
		if q, err = b.questionFromTranscript(ctx, requestID); err != nil {
			return err
		}
	}
	if !reject && len(answers) != len(q.questions) {
		return fmt.Errorf("question %q expects %d answers, got %d", requestID, len(q.questions), len(answers))
	}

	if q.viaPerm {
		// The parked handleCanUseTool owns cleanup.
		msg := questionsDismissedMessage
		if !reject {
			msg = formatQuestionAnswers(q.questions, answers)
		}
		return b.RespondPermission(ctx, requestID, false, msg)
	}

	b.mu.Lock()
	delete(b.pendingQuestions, requestID)
	b.mu.Unlock()
	if reject {
		return nil
	}
	return b.Send(ctx, SendMessageOpts{Text: formatQuestionAnswers(q.questions, answers)})
}

// questionFromTranscript recovers a bypass question's definition from the
// on-disk transcript by its "q-<tool_use_id>" request id. This is the
// restart-proof path: the in-memory pendingQuestions map dies with the
// process, but the tagged tool part is durable in the transcript.
func (b *ClaudeCodeBackend) questionFromTranscript(ctx context.Context, requestID string) (claudeQuestion, error) {
	toolUseID, ok := strings.CutPrefix(requestID, bypassQuestionIDPrefix)
	if !ok {
		return claudeQuestion{}, fmt.Errorf("no pending question %q", requestID)
	}
	msgs, err := b.Messages(ctx)
	if err != nil {
		return claudeQuestion{}, fmt.Errorf("no pending question %q (transcript lookup: %w)", requestID, err)
	}
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.ID == toolUseID && p.Question != nil {
				return claudeQuestion{toolUseID: toolUseID, questions: p.Question.Questions}, nil
			}
		}
	}
	return claudeQuestion{}, fmt.Errorf("no pending question %q", requestID)
}

// failPendingPermissions denies every parked permission request. Called on
// Abort so the SDK read goroutine is freed immediately rather than waiting for
// the interrupt to propagate through the CLI. Stop relies on b.ctx cancellation
// instead, which handleCanUseTool also selects on.
func (b *ClaudeCodeBackend) failPendingPermissions() {
	b.mu.Lock()
	waiters := make([]chan permissionDecision, 0, len(b.pendingPerms))
	for _, p := range b.pendingPerms {
		waiters = append(waiters, p.decision)
	}
	b.mu.Unlock()

	for _, ch := range waiters {
		select {
		case ch <- permissionDecision{allow: false, denyMessage: "aborted"}:
		default:
		}
	}
}

// describeToolCall renders a short, human-readable summary of a tool request
// for the permission prompt, mirroring the OpenCode backend's style. It picks
// the single most salient input field and caps length so a large input doesn't
// bloat the prompt.
func describeToolCall(tool string, input map[string]any) string {
	var detail string
	switch {
	case tool == ClaudeToolAskUserQuestion:
		// Show the first question instead of the bare tool name (which renders
		// as the unhelpful "AskUserQuestion: AskUserQuestion" on old clients).
		if qs := parseClaudeQuestions(input); len(qs) > 0 {
			detail = qs[0].Text
		}
	case input["command"] != nil:
		detail = fmt.Sprint(input["command"])
	case input["file_path"] != nil:
		detail = fmt.Sprint(input["file_path"])
	case input["path"] != nil:
		detail = fmt.Sprint(input["path"])
	case input["url"] != nil:
		detail = fmt.Sprint(input["url"])
	}

	detail = strings.TrimSpace(strings.ReplaceAll(detail, "\n", " "))
	const maxDetail = 120
	if len(detail) > maxDetail {
		detail = detail[:maxDetail] + "…"
	}
	if detail == "" {
		return tool
	}
	return tool + ": " + detail
}
