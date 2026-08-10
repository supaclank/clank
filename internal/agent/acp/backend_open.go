package acp

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/supaclank/clank/internal/agent"
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
		// The loaded transcript is the session's memory, but the agent
		// process behind it is freshly spawned with its own default
		// mode/model/effort. Re-assert the last-applied config so a
		// rehydrate can't silently downgrade the session's policy (e.g.
		// bypassPermissions → prompt-mode, stalling unattended runs).
		b.reassertConfig(ctx, conn)
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

// reassertConfig re-applies the session's last-applied config after
// session/load: mode first through applyMode (whose advertised-list and
// already-current guards make an agent that preserved state a no-op),
// the rest through session/set_config_option in sorted order — the same
// channels and posture as applyConfig, and like it advisory-only:
// failures log, never fail the Open.
func (b *Backend) reassertConfig(ctx context.Context, conn *AdapterConn) {
	b.mu.Lock()
	cfg := maps.Clone(b.lastConfig)
	b.mu.Unlock()
	if len(cfg) == 0 {
		return
	}
	if mode := cfg[agent.ConfigOptionMode]; mode != "" {
		b.applyMode(ctx, conn, mode)
	}
	rest := make([]string, 0, len(cfg))
	for id := range cfg {
		if id != agent.ConfigOptionMode && id != "" {
			rest = append(rest, id)
		}
	}
	slices.Sort(rest)
	for _, id := range rest {
		if cfg[id] != "" {
			b.setConfigValue(ctx, conn, id, cfg[id])
		}
	}
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
	// TODO(ai-review): opencode advertises modes via the config-option channel; verify it honors set_mode here or add a SetSessionConfigOption apply path like models have. https://github.com/supaclank/clank/pull/188
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
// modes plus the currently active id, untranslated. currentMode is
// maintained by session/new + session/load responses, applyMode, and
// live current_mode_update notifications (HandleSessionUpdate).
func (b *Backend) Modes() (string, []agent.SessionMode) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentMode, slices.Clone(b.availableModes)
}

// applySessionStateLocked records the agent-advertised session state
// that session/new, session/load, and config_option_update all carry:
// modes, the model list, and the full config-option set. Callers hold
// b.mu.
func (b *Backend) applySessionStateLocked(modes *sdk.SessionModeState, opts []sdk.SessionConfigOption) {
	// Two channels carry the same thing and agents differ in which they
	// use: claude-agent-acp sends a SessionModeState AND a "mode" config
	// option; `opencode acp` sends ONLY the config option (its agents).
	// Reading just the state left opencode with no mode list at all.
	if modes != nil {
		b.currentMode = string(modes.CurrentModeId)
		b.availableModes = modesFromState(modes)
	} else if sel := selectByCategory(opts, modeConfigOptionID); sel != nil {
		b.currentMode = string(sel.CurrentValue)
		b.availableModes = modesFromSelect(sel)
	}
	if current, models, ok := modelsFromConfigOptions(opts); ok {
		b.currentModel = current
		b.availableModels = models
	}
	b.configOptions = configOptionsFromACP(opts)
	// An agent that expresses mode ONLY as SessionModeState still yields
	// one uniform knob list. Sourced from the retained fields so a later
	// config_option_update (which carries no mode state) can't lose it.
	if selectByCategory(opts, modeConfigOptionID) == nil && len(b.availableModes) > 0 {
		b.configOptions = append([]agent.ConfigOption{synthesizedModeOption(b.currentMode, b.availableModes)}, b.configOptions...)
	}
}

// configOptionsFromACP converts advertised config options to the wire
// type verbatim, flattening grouped values (group label retained).
func configOptionsFromACP(opts []sdk.SessionConfigOption) []agent.ConfigOption {
	out := make([]agent.ConfigOption, 0, len(opts))
	for _, o := range opts {
		sel := o.Select
		if sel == nil {
			continue // non-select option kinds: nothing selectable to offer
		}
		co := agent.ConfigOption{
			ID:           string(sel.Id),
			Name:         sel.Name,
			CurrentValue: string(sel.CurrentValue),
		}
		if sel.Category != nil {
			co.Category = string(*sel.Category)
		}
		if sel.Description != nil {
			co.Description = *sel.Description
		}
		switch {
		case sel.Options.Ungrouped != nil:
			for _, it := range *sel.Options.Ungrouped {
				co.Values = append(co.Values, configValueFromACP(it, ""))
			}
		case sel.Options.Grouped != nil:
			for _, g := range *sel.Options.Grouped {
				for _, it := range g.Options {
					co.Values = append(co.Values, configValueFromACP(it, g.Name))
				}
			}
		}
		out = append(out, co)
	}
	return out
}

func configValueFromACP(it sdk.SessionConfigSelectOption, group string) agent.ConfigOptionValue {
	v := agent.ConfigOptionValue{Value: string(it.Value), Name: it.Name, Group: group}
	if it.Description != nil {
		v.Description = *it.Description
	}
	return v
}

// synthesizedModeOption presents SessionModeState as a config option.
func synthesizedModeOption(current string, modes []agent.SessionMode) agent.ConfigOption {
	co := agent.ConfigOption{
		ID:           modeConfigOptionID,
		Name:         "Mode",
		Category:     modeConfigOptionID,
		CurrentValue: current,
	}
	for _, m := range modes {
		co.Values = append(co.Values, agent.ConfigOptionValue{Value: m.ID, Name: m.Name, Description: m.Description})
	}
	return co
}

// selectByCategory finds the select config option for a semantic
// category ("model", "mode"), falling back to the conventional id for
// agents that omit the category.
func selectByCategory(opts []sdk.SessionConfigOption, category string) *sdk.SessionConfigOptionSelect {
	for _, o := range opts {
		sel := o.Select
		if sel == nil {
			continue
		}
		if sel.Category != nil {
			if string(*sel.Category) == category {
				return sel
			}
			continue
		}
		if string(sel.Id) == category {
			return sel
		}
	}
	return nil
}

// modesFromSelect maps a "mode"-category config option onto clank's
// agent-owned mode type.
func modesFromSelect(sel *sdk.SessionConfigOptionSelect) []agent.SessionMode {
	items := selectItems(sel.Options)
	out := make([]agent.SessionMode, 0, len(items))
	for _, item := range items {
		desc := ""
		if item.Description != nil {
			desc = *item.Description
		}
		out = append(out, agent.SessionMode{ID: string(item.Value), Name: item.Name, Description: desc})
	}
	return out
}

// modelsFromConfigOptions extracts the model picker from the agent's
// session config options. ok is false when the agent advertises no
// model choice.
func modelsFromConfigOptions(opts []sdk.SessionConfigOption) (current string, models []agent.ModelInfo, ok bool) {
	sel := selectByCategory(opts, modelConfigOptionID)
	if sel == nil {
		return "", nil, false
	}
	for _, item := range selectItems(sel.Options) {
		models = append(models, agent.ModelInfo{ID: string(item.Value), Name: item.Name})
	}
	return string(sel.CurrentValue), models, true
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

// ConfigOptions implements agent.ConfigOptionsReporter: the agent's
// full advertised config knobs, untranslated. Deep-cloned (including each
// option's Values) so callers can't mutate retained backend state.
func (b *Backend) ConfigOptions() []agent.ConfigOption {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]agent.ConfigOption, len(b.configOptions))
	for i, co := range b.configOptions {
		out[i] = co
		out[i].Values = slices.Clone(co.Values)
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
