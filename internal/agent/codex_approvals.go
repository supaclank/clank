package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	codex "github.com/pmenglund/codex-sdk-go"
	"github.com/pmenglund/codex-sdk-go/protocol"
)

// Codex approval decision wire values (item/*/requestApproval responses).
const (
	codexDecisionAccept  = "accept"
	codexDecisionDecline = "decline"
)

// codexApprovals implements rpc.ServerRequestHandler for a backend. The three
// side-effect approvals (command execution, file changes, permission
// escalations) park on clank's permission UI; everything else inherits
// AutoApproveHandler's defaults — those requests (dynamic tools, MCP
// elicitation, auth-token refresh) only occur with configurations clank
// doesn't set up, and auto-answering keeps the protocol from wedging if one
// ever arrives.
type codexApprovals struct {
	codex.AutoApproveHandler
	b *CodexBackend
}

// ItemCommandExecutionRequestApproval parks a shell-command approval on the
// permission UI. Runs on the SDK's server-request goroutine; blocking here is
// safe because codex itself is paused awaiting the decision (Abort frees the
// park via failPendingPermissions before interrupting).
func (h *codexApprovals) ItemCommandExecutionRequestApproval(ctx context.Context, params protocol.CommandExecutionRequestApprovalParams) (*protocol.CommandExecutionRequestApprovalResponse, error) {
	desc := codexToolShell
	if params.Command != nil && *params.Command != "" {
		desc = codexPermissionDetail(codexToolShell, *params.Command)
	}
	allow := h.b.parkPermission(ctx, codexToolShell, desc, params.ItemID)
	return &protocol.CommandExecutionRequestApprovalResponse{Decision: codexDecision(allow)}, nil
}

// ItemFileChangeRequestApproval parks a patch-application approval.
func (h *codexApprovals) ItemFileChangeRequestApproval(ctx context.Context, params protocol.FileChangeRequestApprovalParams) (*protocol.FileChangeRequestApprovalResponse, error) {
	desc := codexToolFileChange
	if params.Reason != nil && *params.Reason != "" {
		desc = codexPermissionDetail(codexToolFileChange, *params.Reason)
	}
	allow := h.b.parkPermission(ctx, codexToolFileChange, desc, params.ItemID)
	return &protocol.FileChangeRequestApprovalResponse{Decision: codexDecision(allow)}, nil
}

// ItemPermissionsRequestApproval parks a permission-escalation request (e.g.
// network access from inside the sandbox). Approval echoes the requested
// permissions back, mirroring AutoApproveHandler's accept shape; denial
// returns an empty grant.
func (h *codexApprovals) ItemPermissionsRequestApproval(ctx context.Context, params protocol.PermissionsRequestApprovalParams) (*protocol.PermissionsRequestApprovalResponse, error) {
	desc := codexToolPermissions
	if params.Reason != nil && *params.Reason != "" {
		desc = codexPermissionDetail(codexToolPermissions, *params.Reason)
	} else if params.Permissions != nil {
		if raw, err := json.Marshal(params.Permissions); err == nil {
			desc = codexPermissionDetail(codexToolPermissions, string(raw))
		}
	}
	allow := h.b.parkPermission(ctx, codexToolPermissions, desc, params.ItemID)
	if !allow {
		return &protocol.PermissionsRequestApprovalResponse{}, nil
	}
	return &protocol.PermissionsRequestApprovalResponse{Permissions: params.Permissions}, nil
}

// codexDecision maps the user's choice to the approval wire value.
func codexDecision(allow bool) string {
	if allow {
		return codexDecisionAccept
	}
	return codexDecisionDecline
}

// codexPermissionDetail renders "tool: detail" capped like Claude's
// describeToolCall so large inputs don't bloat the prompt.
func codexPermissionDetail(tool, detail string) string {
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

// parkPermission emits an EventPermission and blocks until RespondPermission
// delivers the decision (or the session aborts/stops → deny). itemID rides
// as ToolUseID so clients correlate the prompt with the already-rendered
// tool card (codex emits item/started before requesting approval).
func (b *CodexBackend) parkPermission(ctx context.Context, tool, description, itemID string) bool {
	decision := make(chan permissionDecision, 1)

	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return false
	}
	b.permSeq++
	id := fmt.Sprintf("perm-%d", b.permSeq)
	b.pendingPerms[id] = decision
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
			Description: description,
			ToolUseID:   itemID,
		},
	})

	select {
	case d := <-decision:
		return d.allow
	case <-ctx.Done():
		return false
	case <-b.ctx.Done():
		return false
	}
}

// RespondPermission delivers the user's decision to a parked approval.
// Codex approvals have no deny-reason field, so denyMessage is accepted but
// unused (same contract as the OpenCode backend). Returns an error for an
// unknown id so callers fail fast on a stale prompt.
func (b *CodexBackend) RespondPermission(_ context.Context, permissionID string, allow bool, denyMessage string) error {
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

// RespondQuestion is unsupported: codex has no AskUserQuestion analog on the
// stable surface (item/tool/requestUserInput exists but clank doesn't
// configure tools that use it yet).
func (b *CodexBackend) RespondQuestion(context.Context, string, []QuestionAnswer, bool) error {
	return fmt.Errorf("codex backend does not support question prompts")
}

// failPendingPermissions denies every parked approval. Called on Abort (and
// Stop) so the SDK's server-request goroutine is freed before an interrupt
// round-trip needs the connection.
func (b *CodexBackend) failPendingPermissions() {
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
