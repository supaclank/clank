package acp

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/acksell/clank/internal/agent"
	sdk "github.com/coder/acp-go-sdk"
)

// atomicTime tracks the last session/update arrival for the turn drain.
type atomicTime struct{ v atomic.Int64 }

func (t *atomicTime) set(now time.Time) { t.v.Store(now.UnixNano()) }
func (t *atomicTime) get() time.Time    { return time.Unix(0, t.v.Load()) }

// Open establishes the ACP session: session/new for fresh sessions,
// session/load (full replay into the reducer, no events) for resumes.
// Idempotent; safe to call on every dispatch like the host does.
func (b *Backend) Open(ctx context.Context) error {
	b.openMu.Lock()
	defer b.openMu.Unlock()

	b.mu.Lock()
	if b.stopping {
		b.mu.Unlock()
		return fmt.Errorf("acp %s: backend stopped", b.profile.ID)
	}
	if b.opened && b.conn != nil && connAlive(b.conn) {
		b.mu.Unlock()
		return nil
	}
	resume := b.sessionID
	guidance := b.guidance
	b.mu.Unlock()

	conn, err := b.resolver(ctx)
	if err != nil {
		return fmt.Errorf("acp %s: adapter unavailable: %w", b.profile.ID, err)
	}

	if resume == "" {
		var meta map[string]any
		if b.profile.SessionNewMeta != nil {
			meta = b.profile.SessionNewMeta(guidance)
		}
		ns, err := conn.Conn().NewSession(ctx, sdk.NewSessionRequest{
			Cwd:        b.workDir,
			McpServers: []sdk.McpServer{},
			Meta:       meta,
		})
		if err != nil {
			return fmt.Errorf("acp %s: session/new: %w", b.profile.ID, err)
		}
		sid := string(ns.SessionId)
		conn.Register(ns.SessionId, b)

		b.mu.Lock()
		b.sessionID = sid
		b.red.setSessionID(sid)
		if ns.Modes != nil {
			b.currentMode = string(ns.Modes.CurrentModeId)
			b.availableModes = modesFromState(ns.Modes)
		}
		b.mu.Unlock()

		if b.initialMode != "" {
			b.applyMode(ctx, conn, string(b.initialMode))
		}
	} else {
		conn.Register(sdk.SessionId(resume), b)
		b.mu.Lock()
		b.red.setSessionID(resume)
		preLoadCount := b.red.messageCount()
		preTurnSeq := b.red.turnSeq
		b.red.replaying = true
		b.mu.Unlock()

		_, err := conn.Conn().LoadSession(ctx, sdk.LoadSessionRequest{
			SessionId:  sdk.SessionId(resume),
			Cwd:        b.workDir,
			McpServers: []sdk.McpServer{},
		})
		b.mu.Lock()
		if err != nil {
			// Updates may have streamed in before the RPC failed; discard
			// them so a retried Open doesn't duplicate replayed history.
			b.red.rollbackReplay(preLoadCount, preTurnSeq)
		} else {
			b.red.finishReplay()
		}
		b.mu.Unlock()
		if err != nil {
			conn.Deregister(sdk.SessionId(resume))
			return fmt.Errorf("acp %s: session/load %s: %w", b.profile.ID, resume, err)
		}
	}

	b.mu.Lock()
	b.conn = conn
	b.opened = true
	if b.status == agent.StatusStarting {
		b.setStatusLocked(agent.StatusIdle)
	}
	b.mu.Unlock()
	go b.watchConn(conn)
	return nil
}

// OpenAndSend is Open followed by Send — ACP has no fused primitive.
func (b *Backend) OpenAndSend(ctx context.Context, opts agent.SendMessageOpts) error {
	if err := b.Open(ctx); err != nil {
		return err
	}
	return b.Send(ctx, opts)
}

// applyMode sends session/set_mode with the agent-owned mode id as-is.
// When the agent advertised a mode list, unknown ids are skipped (a
// stale client selection must not flip the session into an error state);
// with no advertised list the id is sent optimistically. Failures log
// rather than fail the session (mode is advisory UX).
func (b *Backend) applyMode(ctx context.Context, conn *AdapterConn, modeID string) {
	b.mu.Lock()
	sid := b.sessionID
	current := b.currentMode
	advertised := b.availableModes
	b.mu.Unlock()
	if sid == "" || modeID == "" || modeID == current {
		return
	}
	if len(advertised) > 0 && !slices.ContainsFunc(advertised, func(m agent.SessionMode) bool { return m.ID == modeID }) {
		b.logf("acp %s: skipping set_mode %q: not advertised by the agent", b.profile.ID, modeID)
		return
	}
	_, err := conn.Conn().SetSessionMode(ctx, sdk.SetSessionModeRequest{
		SessionId: sdk.SessionId(sid),
		ModeId:    sdk.SessionModeId(modeID),
	})
	if err != nil {
		b.logf("acp %s: set_mode %s: %v", b.profile.ID, modeID, err)
		return
	}
	b.mu.Lock()
	b.currentMode = modeID
	b.mu.Unlock()
}

// Modes implements agent.ModeReporter: the agent-advertised session
// modes plus the currently active id, untranslated.
func (b *Backend) Modes() (string, []agent.SessionMode) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.red.modeID; m != "" {
		b.currentMode = m
	}
	return b.currentMode, slices.Clone(b.availableModes)
}

// modesFromState maps the SDK mode state onto clank's agent-owned type.
func modesFromState(st *sdk.SessionModeState) []agent.SessionMode {
	out := make([]agent.SessionMode, 0, len(st.AvailableModes))
	for _, m := range st.AvailableModes {
		desc := ""
		if m.Description != nil {
			desc = *m.Description
		}
		out = append(out, agent.SessionMode{ID: string(m.Id), Name: m.Name, Description: desc})
	}
	return out
}

func connAlive(c *AdapterConn) bool {
	select {
	case <-c.Closed():
		return false
	default:
		return true
	}
}
