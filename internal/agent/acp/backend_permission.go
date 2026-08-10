package acp

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/supaclank/clank/internal/agent"
	sdk "github.com/coder/acp-go-sdk"
)

// permIDPrefix namespaces parked-permission request ids so they can't
// collide with other id spaces (e.g. session/message ids).
const permIDPrefix = "perm-"

// HandleSessionUpdate implements SessionHandler: reduce, then emit.
func (b *Backend) HandleSessionUpdate(_ context.Context, n sdk.SessionNotification) {
	b.lastUpdate.set(time.Now())
	b.mu.Lock()
	defer b.mu.Unlock()
	if cu := n.Update.ConfigOptionUpdate; cu != nil {
		// Carries the FULL option set; keep the retained knobs current.
		b.applySessionStateLocked(nil, cu.ConfigOptions)
	}
	if mu := n.Update.CurrentModeUpdate; mu != nil && !b.red.replaying {
		// A live agent-initiated mode change (e.g. plan approval flips
		// plan → default). Folding it into lastConfig keeps the re-assert
		// truthful: restoring the pre-approval mode would drop a built
		// session back into plan. Replayed mode updates are history — the
		// session/load response carries the authoritative current mode.
		if id := string(mu.CurrentModeId); id != "" && id != b.currentMode {
			b.currentMode = id
			b.reflectAppliedModeLocked(id)
			b.recordConfigLocked(agent.ConfigOptionMode, id)
			b.emitLocked(agent.Event{Type: agent.EventModeChange, Data: agent.ModeChangeData{ModeID: id}})
		}
	}
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
	requestID := fmt.Sprintf("%s%d", permIDPrefix, b.permSeq)
	if toolUseID != "" {
		requestID = permIDPrefix + toolUseID
	}
	title := ""
	if req.ToolCall.Title != nil {
		title = *req.ToolCall.Title
	}
	data := agent.PermissionData{
		RequestID:   requestID,
		Tool:        toolName(title, req.ToolCall.Meta),
		Description: title,
		ToolUseID:   toolUseID,
	}
	ch := make(chan permDecision, 1)
	b.pendingPerms[requestID] = parkedPermission{seq: b.permSeq, data: data, ch: ch}
	b.emitLocked(agent.Event{Type: agent.EventPermission, Data: data})
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

// RespondPermission resolves a parked request. A denyMessage becomes a
// follow-up prompt: ACP permission outcomes carry an option id and nothing
// else, so the reason reaches the model as the user's next message. That is
// how plan revision works — rejecting ExitPlanMode keeps the session in plan
// mode and ends the turn, and the queued message asks for the changes.
// Ignored when allow is true (a granted permission has no reason to carry).
func (b *Backend) RespondPermission(ctx context.Context, permissionID string, allow bool, denyMessage string) error {
	b.mu.Lock()
	p, ok := b.pendingPerms[permissionID]
	if ok {
		delete(b.pendingPerms, permissionID)
	}
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("acp %s: unknown permission request %q", b.profile.ID, permissionID)
	}
	p.ch <- permDecision{allow: allow}
	close(p.ch)

	if allow || denyMessage == "" {
		return nil
	}
	// bgCtx, not ctx: the follow-up must outlive the reply request that
	// carried it. Send queues behind the turn this denial is ending, so it
	// dispatches once the agent settles.
	if err := b.Send(b.bgCtx, agent.SendMessageOpts{Text: denyMessage}); err != nil {
		// The denial already landed; surface the message failure rather than
		// dropping the user's text silently.
		return fmt.Errorf("acp %s: permission denied but follow-up message failed: %w", b.profile.ID, err)
	}
	return nil
}

// PendingPermissions implements agent.PendingPermissionsReporter: the
// requests currently parked in HandleRequestPermission, oldest first, so
// a client that (re)joins mid-block can re-render the prompt it never
// saw on the live stream.
func (b *Backend) PendingPermissions() []agent.PermissionData {
	b.mu.Lock()
	defer b.mu.Unlock()
	parked := slices.SortedFunc(maps.Values(b.pendingPerms), func(x, y parkedPermission) int {
		return cmp.Compare(x.seq, y.seq)
	})
	out := make([]agent.PermissionData, len(parked))
	for i, p := range parked {
		out[i] = p.data
	}
	return out
}

// pickOption maps a binary allow/deny onto the agent's option list:
// allow prefers allow_once, deny prefers reject_once; same-family
// fallback covers agents that only offer the *_always variants.
func pickOption(options []sdk.PermissionOption, allow bool) (sdk.PermissionOptionId, bool) {
	first, second := sdk.PermissionOptionKindRejectOnce, sdk.PermissionOptionKindRejectAlways
	if allow {
		first, second = sdk.PermissionOptionKindAllowOnce, sdk.PermissionOptionKindAllowAlways
	}
	for _, o := range options {
		if o.Kind == first {
			return o.OptionId, true
		}
	}
	for _, o := range options {
		if o.Kind == second {
			return o.OptionId, true
		}
	}
	return "", false
}
