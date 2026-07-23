package acp

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/acksell/clank/internal/agent"
	sdk "github.com/coder/acp-go-sdk"
)

// atomicTime tracks the last session/update arrival for the turn drain.
type atomicTime struct{ v atomic.Int64 }

func (t *atomicTime) set(now time.Time)       { t.v.Store(now.UnixNano()) }
func (t *atomicTime) get() time.Time          { return time.Unix(0, t.v.Load()) }
func (t *atomicTime) sinceSet() time.Duration { return time.Since(t.get()) }

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
		}
		b.mu.Unlock()

		if b.initialMode != "" {
			b.applyMode(ctx, conn, b.initialMode)
		}
	} else {
		conn.Register(sdk.SessionId(resume), b)
		b.mu.Lock()
		b.red.setSessionID(resume)
		b.red.replaying = true
		b.mu.Unlock()

		_, err := conn.Conn().LoadSession(ctx, sdk.LoadSessionRequest{
			SessionId:  sdk.SessionId(resume),
			Cwd:        b.workDir,
			McpServers: []sdk.McpServer{},
		})
		b.mu.Lock()
		b.red.finishReplay()
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

// applyMode sends session/set_mode when the profile maps the clank mode;
// failures log rather than fail the session (mode is advisory UX).
func (b *Backend) applyMode(ctx context.Context, conn *AdapterConn, mode agent.ClaudePermissionMode) {
	if b.profile.ModeFor == nil {
		return
	}
	modeID, ok := b.profile.ModeFor(mode)
	if !ok {
		return
	}
	b.mu.Lock()
	sid := b.sessionID
	current := b.currentMode
	b.mu.Unlock()
	if sid == "" || modeID == current {
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

func connAlive(c *AdapterConn) bool {
	select {
	case <-c.Closed():
		return false
	default:
		return true
	}
}
