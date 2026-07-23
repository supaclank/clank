package acp

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/agent"
	sdk "github.com/coder/acp-go-sdk"
)

// maxQueuedPrompts bounds the send-while-busy FIFO (user decision:
// queue, don't reject — busy holds until the queue drains).
const maxQueuedPrompts = 8

// Send resolves attachments, applies mode/model changes, records + emits
// the user message, and enqueues the prompt for the turn runner. ACP is
// one-turn-at-a-time, so prompts dispatch sequentially.
func (b *Backend) Send(ctx context.Context, opts agent.SendMessageOpts) error {
	b.mu.Lock()
	if !b.opened || b.conn == nil {
		b.mu.Unlock()
		return fmt.Errorf("acp %s: backend not open", b.profile.ID)
	}
	if b.stopping {
		b.mu.Unlock()
		return fmt.Errorf("acp %s: backend stopped", b.profile.ID)
	}
	if len(b.queue) >= maxQueuedPrompts {
		b.mu.Unlock()
		return fmt.Errorf("acp %s: prompt queue full (%d queued)", b.profile.ID, maxQueuedPrompts)
	}
	conn := b.conn
	b.mu.Unlock()

	// Attachments resolve synchronously so failures surface to the caller
	// before the session flips busy (mirrors the bespoke backends).
	images, err := agent.ResolveAttachments(ctx, opts.Attachments)
	if err != nil {
		return err
	}

	if opts.PermissionMode != "" {
		b.applyMode(ctx, conn, string(opts.PermissionMode))
	}
	if opts.Model != nil && b.profile.ModelOption != nil {
		if id, value, ok := b.profile.ModelOption(*opts.Model); ok {
			b.setConfigValue(ctx, conn, id, value)
		}
	}

	blocks := make([]sdk.ContentBlock, 0, 1+len(images))
	if opts.Text != "" {
		blocks = append(blocks, sdk.TextBlock(opts.Text))
	}
	for _, img := range images {
		blocks = append(blocks, sdk.ImageBlock(base64.StdEncoding.EncodeToString(img.Data), img.Mime))
	}
	if len(blocks) == 0 {
		return fmt.Errorf("acp %s: empty message", b.profile.ID)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopping {
		return fmt.Errorf("acp %s: backend stopped", b.profile.ID)
	}
	if b.status == agent.StatusDead {
		// watchConn can mark the session dead while attachments/mode/model
		// calls above were in flight; don't resurrect it to Busy.
		return fmt.Errorf("acp %s: backend dead", b.profile.ID)
	}
	b.userSeq++
	userMsg := agent.MessageData{
		ID:      fmt.Sprintf("%s:u%d", b.sessionID, b.userSeq),
		Role:    "user",
		Content: opts.Text,
	}
	b.red.appendUserMessage(userMsg)
	b.emitLocked(agent.Event{Type: agent.EventMessage, Data: userMsg})
	b.queue = append(b.queue, queuedPrompt{blocks: blocks})
	b.setStatusLocked(agent.StatusBusy)
	if !b.runnerOn {
		b.runnerOn = true
		go b.runTurns()
	}
	return nil
}

// Abort cancels the in-flight turn (session/cancel), drops queued
// prompts, and resolves parked permissions as cancelled. The prompt
// response arrives with stopReason=cancelled and settles to idle.
func (b *Backend) Abort(ctx context.Context) error {
	b.mu.Lock()
	conn := b.conn
	sid := b.sessionID
	hadTurn := b.runnerOn
	b.aborting = true
	b.queue = nil
	b.failPendingPermsLocked()
	b.mu.Unlock()

	if conn == nil || sid == "" {
		// No turn is running (and none can be, without a conn/session), so
		// there's nothing for runTurns' cleanup to reset — do it here.
		// Otherwise b.aborting sticks forever and swallows the next
		// genuine turn's RPC error as a false cancellation.
		b.mu.Lock()
		b.aborting = false
		b.mu.Unlock()
		return nil
	}
	if err := conn.Conn().Cancel(ctx, sdk.CancelNotification{SessionId: sdk.SessionId(sid)}); err != nil {
		if !hadTurn {
			// No runTurns cleanup will fire for this call — reset here so
			// a failed Cancel doesn't stick b.aborting forever (same
			// hazard as the conn==nil case above).
			b.mu.Lock()
			b.aborting = false
			b.mu.Unlock()
		}
		return fmt.Errorf("acp %s: session/cancel: %w", b.profile.ID, err)
	}
	if !hadTurn {
		// Nothing in flight — settle immediately.
		b.mu.Lock()
		b.aborting = false
		if b.status == agent.StatusBusy {
			b.setStatusLocked(agent.StatusIdle)
		}
		b.mu.Unlock()
	}
	return nil
}

// shouldSwallowPromptErrLocked reports whether a Prompt RPC error should
// fold into the cancellation path (queue cleared, no EventError/StatusError)
// rather than surface as a turn failure. Besides the usual abort/stop
// cases, an already-Dead status means watchConn saw the transport die
// first — the error is just that same death observed from runTurns'
// side, and StatusError must not regress a Dead session back to Error.
func (b *Backend) shouldSwallowPromptErrLocked(aborting bool) bool {
	return aborting || b.stopping || b.status == agent.StatusDead
}

// runTurns is the single per-backend turn runner: it dispatches queued
// prompts sequentially, drains the late-update window after each
// response, commits the turn, and settles status when the queue empties.
func (b *Backend) runTurns() {
	for {
		b.mu.Lock()
		if b.stopping || len(b.queue) == 0 {
			b.runnerOn = false
			b.aborting = false
			if !b.stopping && b.status == agent.StatusBusy {
				b.setStatusLocked(agent.StatusIdle)
			}
			b.mu.Unlock()
			return
		}
		item := b.queue[0]
		b.queue = b.queue[1:]
		conn := b.conn
		sid := b.sessionID
		b.red.beginTurn()
		b.mu.Unlock()

		// Captured before dispatch: the drain floors its quiet window on
		// this, so a turn whose updates lag the response still gets one.
		turnStart := time.Now()
		resp, err := conn.Conn().Prompt(b.bgCtx, sdk.PromptRequest{
			SessionId: sdk.SessionId(sid),
			Prompt:    item.blocks,
		})
		b.drainLateUpdates(turnStart)

		b.mu.Lock()
		for _, e := range b.red.finishTurn() {
			b.emitLocked(e)
		}
		aborting := b.aborting
		switch {
		case err != nil && b.shouldSwallowPromptErrLocked(aborting):
			// Cancellation surfaces as an RPC error on some adapters;
			// treat like stopReason=cancelled.
			b.queue = nil
		case err != nil:
			b.queue = nil
			b.emitLocked(agent.Event{Type: agent.EventError, Data: agent.ErrorData{Message: err.Error()}})
			b.setStatusLocked(agent.StatusError)
			b.runnerOn = false
			b.aborting = false
			b.mu.Unlock()
			return
		case resp.StopReason == sdk.StopReasonRefusal:
			// The turn ended cleanly; surface the refusal as information.
			b.emitLocked(agent.Event{Type: agent.EventError, Data: agent.ErrorData{Message: "the agent refused to continue this turn"}})
		}
		b.mu.Unlock()
	}
}

// drainLateUpdates waits for this turn's update stream to go quiet so
// trailing tool_call_updates land in the turn they belong to (claude
// #864 class). Session-scoped updates (title, mode) are safe at any time
// and don't need this window.
//
// The quiet window is measured from max(lastUpdate, turnStart), never
// from lastUpdate alone: that timestamp is session-scoped, so on a turn
// whose own updates haven't been processed yet it still holds a stale
// (or zero) value and an unfloored comparison would exit the drain
// instantly. That is not hypothetical — notification dispatch races the
// prompt response, so on a loaded machine the very first turn of a
// session would commit before its tool_call arrived, then attribute the
// tool's updates to a phantom follow-up turn and leak part events after
// idle. The floor also covers a queued turn that follows a >drainQuiet
// gap with no updates of its own.
//
// TODO(ai-review): switch the 25ms poll below to a timer/cond-based wait
// to cut wakeups during the drain window. https://github.com/Acksell/clank/pull/185
func (b *Backend) drainLateUpdates(turnStart time.Time) {
	deadline := time.Now().Add(drainCap)
	for time.Now().Before(deadline) {
		quietSince := b.lastUpdate.get()
		if quietSince.Before(turnStart) {
			quietSince = turnStart
		}
		if time.Since(quietSince) >= drainQuiet {
			return
		}
		select {
		case <-b.bgCtx.Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (b *Backend) setConfigValue(ctx context.Context, conn *AdapterConn, id, value string) {
	b.mu.Lock()
	sid := b.sessionID
	b.mu.Unlock()
	if sid == "" {
		return
	}
	_, err := conn.Conn().SetSessionConfigOption(ctx, sdk.SetSessionConfigOptionRequest{
		ValueId: &sdk.SetSessionConfigOptionValueId{
			SessionId: sdk.SessionId(sid),
			ConfigId:  sdk.SessionConfigId(id),
			Value:     sdk.SessionConfigValueId(value),
		},
	})
	if err != nil {
		// Config is advisory (model pickers etc.) — log, don't fail the send.
		b.logf("acp %s: set_config_option %s=%s: %v", b.profile.ID, id, value, err)
	}
}
