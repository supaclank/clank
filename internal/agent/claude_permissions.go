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
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pendingPerms, id)
		b.mu.Unlock()
	}()

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
			if tool == "ExitPlanMode" {
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
