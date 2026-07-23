package acp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
	sdk "github.com/coder/acp-go-sdk"
)

// HandleSessionUpdate implements SessionHandler: reduce, then emit.
func (b *Backend) HandleSessionUpdate(_ context.Context, n sdk.SessionNotification) {
	b.lastUpdate.set(time.Now())
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.red.reduce(n) {
		b.emitLocked(e)
	}
}

// HandleRequestPermission implements SessionHandler: park the agent's
// request, surface EventPermission, and block until RespondPermission
// (or an abort/stop/disconnect sweep) decides — the same parking
// contract as the bespoke claude CanUseTool bridge.
func (b *Backend) HandleRequestPermission(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	toolUseID := string(req.ToolCall.ToolCallId)

	b.mu.Lock()
	if b.stopping {
		b.mu.Unlock()
		return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}, nil
	}
	b.permSeq++
	requestID := fmt.Sprintf("perm-%d", b.permSeq)
	if toolUseID != "" {
		requestID = "perm-" + toolUseID
	}
	ch := make(chan permDecision, 1)
	b.pendingPerms[requestID] = ch
	title := ""
	if req.ToolCall.Title != nil {
		title = *req.ToolCall.Title
	}
	b.emitLocked(agent.Event{
		Type: agent.EventPermission,
		Data: agent.PermissionData{
			RequestID:   requestID,
			Tool:        toolName(title, req.ToolCall.Meta),
			Description: title,
			ToolUseID:   toolUseID,
		},
	})
	b.mu.Unlock()

	var decision permDecision
	var decided bool
	select {
	case d, ok := <-ch:
		decision, decided = d, ok
	case <-ctx.Done():
	case <-b.bgCtx.Done():
	}

	b.mu.Lock()
	delete(b.pendingPerms, requestID)
	b.mu.Unlock()

	if !decided {
		return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}, nil
	}
	if opt, ok := pickOption(req.Options, decision.allow); ok {
		return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeSelected(opt)}, nil
	}
	return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}, nil
}

// RespondPermission resolves a parked request. denyMessage is dropped:
// ACP permission outcomes carry an option id only (approved cut).
func (b *Backend) RespondPermission(ctx context.Context, permissionID string, allow bool, denyMessage string) error {
	b.mu.Lock()
	ch, ok := b.pendingPerms[permissionID]
	if ok {
		delete(b.pendingPerms, permissionID)
	}
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("acp %s: unknown permission request %q", b.profile.ID, permissionID)
	}
	if denyMessage != "" {
		b.logf("acp %s: deny message dropped (no ACP channel): %q", b.profile.ID, denyMessage)
	}
	ch <- permDecision{allow: allow}
	close(ch)
	return nil
}

// pickOption maps a binary allow/deny onto the agent's option list:
// allow prefers allow_once, deny prefers reject_once; same-family
// fallback covers agents that only offer the *_always variants.
func pickOption(options []sdk.PermissionOption, allow bool) (sdk.PermissionOptionId, bool) {
	prefix := "reject"
	first := sdk.PermissionOptionKindRejectOnce
	if allow {
		prefix = "allow"
		first = sdk.PermissionOptionKindAllowOnce
	}
	for _, o := range options {
		if o.Kind == first {
			return o.OptionId, true
		}
	}
	for _, o := range options {
		if strings.HasPrefix(string(o.Kind), prefix) {
			return o.OptionId, true
		}
	}
	return "", false
}
