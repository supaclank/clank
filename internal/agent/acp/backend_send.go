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
		b.applyMode(ctx, conn, opts.PermissionMode)
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
		return nil
	}
	if err := conn.Conn().Cancel(ctx, sdk.CancelNotification{SessionId: sdk.SessionId(sid)}); err != nil {
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

		resp, err := conn.Conn().Prompt(b.bgCtx, sdk.PromptRequest{
			SessionId: sdk.SessionId(sid),
			Prompt:    item.blocks,
		})
		b.drainLateUpdates()

		b.mu.Lock()
		for _, e := range b.red.finishTurn() {
			b.emitLocked(e)
		}
		aborting := b.aborting
		switch {
		case err != nil && (aborting || b.stopping):
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

// drainLateUpdates waits for the update stream to go quiet so trailing
// tool_call_updates land in the turn they belong to (claude #864 class).
// Session-scoped updates (title, mode) are safe at any time and don't
// need this window.
//
// TODO(ai-review): switch the 25ms poll below to a timer/cond-based wait
// to cut wakeups during the drain window. https://github.com/Acksell/clank/pull/185
func (b *Backend) drainLateUpdates() {
	deadline := time.Now().Add(drainCap)
	for time.Now().Before(deadline) {
		if b.lastUpdate.sinceSet() >= drainQuiet {
			return
		}
		time.Sleep(25 * time.Millisecond)
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
