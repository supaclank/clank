package agent

import (
	"context"
	"fmt"
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
	b.pendingPerms[id] = decision
	toolUseID := b.lastToolUseID[tool]

	// Normalize AskUserQuestion into a structured EventQuestion so clients
	// don't have to sniff tool names out of part input. The permission event
	// still follows (same request id) for clients that predate EventQuestion;
	// question-aware clients suppress it by matching request_id.
	var questions []Question
	if tool == ClaudeToolAskUserQuestion {
		if questions = parseClaudeQuestions(input); questions != nil {
			b.pendingQuestions[id] = claudeQuestion{toolUseID: toolUseID, questions: questions, viaPerm: true}
			if toolUseID != "" {
				b.questionToolUses[toolUseID] = true
			}
		}
	}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pendingPerms, id)
		_, wasQuestion := b.pendingQuestions[id]
		delete(b.pendingQuestions, id)
		b.mu.Unlock()
		// However the prompt resolved (answers, deny, abort), tell every client
		// to clear the question card.
		if wasQuestion {
			b.emit(Event{
				Type:      EventQuestionResolved,
				Timestamp: time.Now(),
				Data:      QuestionResolvedData{RequestID: id},
			})
		}
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
		b.emit(Event{
			Type:      EventPartUpdate,
			Timestamp: time.Now(),
			Data: PartUpdateData{
				Part: Part{
					ID:     toolUseID,
					Type:   PartToolCall,
					Tool:   tool,
					Status: PartRunning,
					Input:  input,
				},
			},
		})
	}

	// Emit the question before the permission so a question-aware client has
	// already registered the request id when the permission arrives and can
	// suppress the redundant generic prompt.
	if questions != nil {
		b.emit(Event{
			Type:      EventQuestion,
			Timestamp: time.Now(),
			Data: QuestionData{
				RequestID: id,
				ToolUseID: toolUseID,
				Questions: questions,
			},
		})
	}

	b.emit(Event{
		Type:      EventPermission,
		Timestamp: time.Now(),
		Data: PermissionData{
			RequestID:   id,
			Tool:        tool,
			Description: describeToolCall(tool, input),
			ToolUseID:   toolUseID,
		},
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
	decision, ok := b.pendingPerms[permissionID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending permission %q", permissionID)
	}
	select {
	case decision <- permissionDecision{allow: allow, denyMessage: denyMessage}:
	default:
	}
	return nil
}

// RespondQuestion delivers structured answers to a pending question prompt.
// For a prompt parked on handleCanUseTool (gated modes) the formatted answers
// ride the permission deny — allow would run the tool's interactive picker
// headless and re-deadlock, so deny-with-message is the only transport that
// reaches the model. For a prompt whose tool auto-ran (bypassPermissions) the
// answers are dispatched as a follow-up user message, mirroring the mobile
// client's fallback.
func (b *ClaudeCodeBackend) RespondQuestion(ctx context.Context, requestID string, answers []QuestionAnswer, reject bool) error {
	b.mu.Lock()
	q, ok := b.pendingQuestions[requestID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending question %q", requestID)
	}
	if !reject && len(answers) != len(q.questions) {
		return fmt.Errorf("question %q expects %d answers, got %d", requestID, len(q.questions), len(answers))
	}

	if q.viaPerm {
		// The parked handleCanUseTool owns cleanup and the resolved event.
		msg := questionsDismissedMessage
		if !reject {
			msg = formatQuestionAnswers(q.questions, answers)
		}
		return b.RespondPermission(ctx, requestID, false, msg)
	}

	b.mu.Lock()
	delete(b.pendingQuestions, requestID)
	b.mu.Unlock()
	b.emit(Event{
		Type:      EventQuestionResolved,
		Timestamp: time.Now(),
		Data:      QuestionResolvedData{RequestID: requestID},
	})
	if reject {
		return nil
	}
	return b.Send(ctx, SendMessageOpts{Text: formatQuestionAnswers(q.questions, answers)})
}

// failPendingPermissions denies every parked permission request. Called on
// Abort so the SDK read goroutine is freed immediately rather than waiting for
// the interrupt to propagate through the CLI. Stop relies on b.ctx cancellation
// instead, which handleCanUseTool also selects on.
func (b *ClaudeCodeBackend) failPendingPermissions() {
	b.mu.Lock()
	waiters := make([]chan permissionDecision, 0, len(b.pendingPerms))
	for _, ch := range b.pendingPerms {
		waiters = append(waiters, ch)
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
