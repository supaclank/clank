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
		b.applySessionStateLocked(ns.Modes, ns.ConfigOptions)
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

		loaded, err := conn.Conn().LoadSession(ctx, sdk.LoadSessionRequest{
			SessionId:  sdk.SessionId(resume),
			Cwd:        b.workDir,
			McpServers: []sdk.McpServer{},
		})
		b.mu.Lock()
		if err == nil {
			// session/load carries the same modes + config options as
			// session/new. Skipping them here left every RESUMED session
			// with no mode picker and no model list — i.e. almost every
			// session a user opens from the inbox.
			b.applySessionStateLocked(loaded.Modes, loaded.ConfigOptions)
		}
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

// applySessionStateLocked records the agent-advertised session state that
// every open path returns: modes (the mode picker) and config options
// (the model picker). Callers hold b.mu.
func (b *Backend) applySessionStateLocked(modes *sdk.SessionModeState, opts []sdk.SessionConfigOption) {
	if modes != nil {
		b.currentMode = string(modes.CurrentModeId)
		b.availableModes = modesFromState(modes)
	}
	if current, models, ok := modelsFromConfigOptions(opts); ok {
		b.currentModel = current
		b.availableModels = models
	}
	if b.onCatalog != nil && len(b.availableModels) > 0 {
		b.onCatalog(b.workDir, b.availableModels)
	}
}

// modelsFromConfigOptions extracts the model picker from the agent's
// session config options: the select option whose category is "model"
// (falling back to the conventional "model" id). ok is false when the
// agent advertises no model choice.
func modelsFromConfigOptions(opts []sdk.SessionConfigOption) (current string, models []agent.ModelInfo, ok bool) {
	for _, o := range opts {
		sel := o.Select
		if sel == nil {
			continue
		}
		isModel := string(sel.Id) == modelConfigOptionID
		if sel.Category != nil {
			isModel = string(*sel.Category) == modelConfigOptionID
		}
		if !isModel {
			continue
		}
		for _, item := range selectItems(sel.Options) {
			models = append(models, agent.ModelInfo{
				ID:   string(item.Value),
				Name: item.Name,
			})
		}
		return string(sel.CurrentValue), models, true
	}
	return "", nil, false
}

// selectItems flattens a select's options, which the protocol allows to
// be either a flat list or grouped under headers.
func selectItems(o sdk.SessionConfigSelectOptions) []sdk.SessionConfigSelectOption {
	if o.Ungrouped != nil {
		return *o.Ungrouped
	}
	var out []sdk.SessionConfigSelectOption
	if o.Grouped != nil {
		for _, g := range *o.Grouped {
			out = append(out, g.Options...)
		}
	}
	return out
}

// Models implements agent.ModelReporter: the agent-advertised model
// choices for this session plus the active one.
func (b *Backend) Models() (string, []agent.ModelInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentModel, slices.Clone(b.availableModels)
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
