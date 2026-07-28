package host

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acptools"
	"github.com/acksell/clank/internal/agent/guidance"
	sdk "github.com/coder/acp-go-sdk"
)

// configOptionsProbeTimeout bounds an on-demand config-options probe: it
// opens one session and reads the advertised options, so a few seconds is
// ample. Past it the adapter is unhealthy and the client retries.
const configOptionsProbeTimeout = 30 * time.Second

// ACPBackendManager adapts one acp.AdapterProfile to agent.BackendManager:
// a supervised adapter process pool plus per-session acp.Backend values.
// One generic implementation serves every ACP agent — per-adapter variance
// lives entirely in the profile.
type ACPBackendManager struct {
	profile acp.AdapterProfile
	sup     *acp.AdapterSupervisor
	envFn   atomic.Pointer[func() map[string]string]

	// probeMu guards the single-flight state for on-demand config-option
	// probes; probeWG lets Shutdown wait out in-flight probes.
	probeMu sync.Mutex
	probing map[string]*configProbe
	probeWG sync.WaitGroup
	closed  bool
}

// ACPDirs locates the on-disk state an ACP manager owns.
type ACPDirs struct {
	// Tools holds the provisioned adapter runtime (bun plus the pinned
	// npm packages). Unused by backends that run the user's own binary.
	Tools string
}

// NewACPBackendManager builds a manager for the given adapter profile.
// The profile's Env is routed through SetEnvResolver so credentials can
// be wired after construction (the AuthManager exists later) and rotated
// at runtime via the supervisor's env-fingerprint restarts.
func NewACPBackendManager(profile acp.AdapterProfile) (*ACPBackendManager, error) {
	m := &ACPBackendManager{
		probing: map[string]*configProbe{},
	}
	scoped := profile.Env
	profile.Env = func(scopeDir string) map[string]string {
		merged := map[string]string{}
		if scoped != nil {
			for k, v := range scoped(scopeDir) {
				merged[k] = v
			}
		}
		if f := m.envFn.Load(); f != nil {
			for k, v := range (*f)() {
				merged[k] = v
			}
		}
		if len(merged) == 0 {
			return nil
		}
		return merged
	}
	sup, err := acp.NewAdapterSupervisor(profile, log.Printf)
	if err != nil {
		return nil, err
	}
	m.profile, m.sup = profile, sup
	return m, nil
}

// NewCodexACPManager builds the codex manager: the pinned codex-acp
// adapter run as plain JS under bun, provisioned lazily into toolsDir on
// first use (host startup never blocks on it, and hosts that never run
// codex never install it). Env arrives via SetEnvResolver (OpenAI sink;
// nil = codex's own ChatGPT login fallback).
func NewCodexACPManager(dirs ACPDirs) (*ACPBackendManager, error) {
	if dirs.Tools == "" {
		return nil, fmt.Errorf("codex acp: tools dir is required")
	}
	var paths atomic.Pointer[acptools.Paths]
	profile := acp.CodexProfile("", "", nil)
	profile.Prepare = func(ctx context.Context, _ string) error {
		p, err := acptools.Ensure(ctx, dirs.Tools)
		if err != nil {
			return err
		}
		paths.Store(&p)
		return nil
	}
	profile.Command = func(string) (string, []string) {
		p := paths.Load() // Prepare ran first (execSpawn ordering)
		return p.BunBin, []string{p.CodexACPEntry}
	}
	return NewACPBackendManager(profile)
}

// NewClaudeACPManager serves claude-code through the pinned
// claude-agent-acp adapter under bun, provisioned lazily into toolsDir
// alongside codex-acp. The adapter's exact-pinned Agent SDK bundles the
// Claude CLI, so the agent version is fixed by the lockfile. Credentials
// arrive via SetEnvResolver (Anthropic sink); the profile adds
// IS_SANDBOX=1 when running as root so bypassPermissions works on
// sprites.
func NewClaudeACPManager(dirs ACPDirs) (*ACPBackendManager, error) {
	if dirs.Tools == "" {
		return nil, fmt.Errorf("claude acp: tools dir is required")
	}
	var paths atomic.Pointer[acptools.Paths]
	profile := acp.ClaudeProfile("", "", nil)
	profile.Prepare = func(ctx context.Context, _ string) error {
		p, err := acptools.Ensure(ctx, dirs.Tools)
		if err != nil {
			return err
		}
		paths.Store(&p)
		return nil
	}
	profile.Command = func(string) (string, []string) {
		p := paths.Load() // Prepare ran first (execSpawn ordering)
		return p.BunBin, []string{p.ClaudeACPEntry}
	}
	return NewACPBackendManager(profile)
}

// NewOpenCodeACPManager serves opencode through `opencode acp` on the
// user's own binary — their install, their state, no version skew clank
// can introduce. Prepare gates on the verified-surface floor (retried
// until it passes) and materializes stack-detected guidance as an
// instructions file inside the worktree's git dir; Env points opencode
// at it via inline config. Guidance is best-effort: it never blocks a
// session.
func NewOpenCodeACPManager() (*ACPBackendManager, error) {
	profile := acp.OpenCodeProfile("opencode")
	var floor onceUntilSuccess
	profile.Prepare = func(ctx context.Context, scopeDir string) error {
		err := floor.do(func() error {
			v, err := agent.OpenCodeVersion(ctx)
			if err != nil {
				return fmt.Errorf("probe opencode version: %w", err)
			}
			ok, err := agent.OpencodeVersionAtLeast(v, agent.PinnedOpencodeVersion)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("opencode %s is older than the verified ACP floor %s — run `opencode upgrade`", v, agent.PinnedOpencodeVersion)
			}
			return nil
		})
		if err != nil {
			return err
		}
		writeOpenCodeGuidance(scopeDir)
		return nil
	}
	profile.Env = opencodeGuidanceEnv
	return NewACPBackendManager(profile)
}

// onceUntilSuccess runs fn on every call until one succeeds, then
// short-circuits forever. Unlike sync.Once, a failing attempt is
// retried on the next call instead of permanently poisoning every
// later caller — e.g. a canceled ctx on the first Prepare() must not
// brick the opencode ACP backend for the rest of the process.
type onceUntilSuccess struct {
	mu   sync.Mutex
	done bool
}

func (o *onceUntilSuccess) do(fn func() error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return nil
	}
	if err := fn(); err != nil {
		return err
	}
	o.done = true
	return nil
}

// opencodeGuidancePath is where materialized guidance lives: inside the
// per-worktree git dir (worktree-id precedent) so it never dirties the
// working tree.
func opencodeGuidancePath(scopeDir string) (string, bool) {
	gitDir, err := agent.GitDir(scopeDir)
	if err != nil {
		return "", false
	}
	return filepath.Join(gitDir, "clank", "guidance.md"), true
}

// writeOpenCodeGuidance materializes (or removes) the guidance file for
// scopeDir. Best-effort by design — a guidance failure must never block
// an agent session.
func writeOpenCodeGuidance(scopeDir string) {
	path, ok := opencodeGuidancePath(scopeDir)
	if !ok {
		return
	}
	text := guidance.Assemble(scopeDir)
	if text == "" {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("[opencode-acp] guidance dir: %v", err)
		return
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		log.Printf("[opencode-acp] guidance write: %v", err)
	}
}

// opencodeGuidanceEnv injects the guidance file as an opencode
// instructions entry via inline config. Keyed on file existence only,
// so guidance content changes don't churn adapter restarts.
func opencodeGuidanceEnv(scopeDir string) map[string]string {
	path, ok := opencodeGuidancePath(scopeDir)
	if !ok {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	cfg, err := json.Marshal(map[string]any{"instructions": []string{path}})
	if err != nil {
		return nil
	}
	return map[string]string{"OPENCODE_CONFIG_CONTENT": string(cfg)}
}

// SetEnvResolver wires credential env for adapter spawns and nudges the
// supervisor so the env-fingerprint restart picks up rotations.
func (m *ACPBackendManager) SetEnvResolver(f func() map[string]string) {
	m.envFn.Store(&f)
	m.sup.Nudge()
}

// BackendType reports which clank backend this manager serves.
func (m *ACPBackendManager) BackendType() agent.BackendType { return m.profile.Backend }

// Supervisor exposes the adapter supervisor for credential-rotation
// nudges and test spawn injection.
func (m *ACPBackendManager) Supervisor() *acp.AdapterSupervisor { return m.sup }

// Init seeds per-dir profiles with known project dirs (host-scoped
// profiles start lazily on first use) and starts the reconciler.
func (m *ACPBackendManager) Init(ctx context.Context, knownDirs func() ([]string, error)) error {
	if m.profile.Scope == acp.ScopePerDir {
		dirs, err := knownDirs()
		if err != nil {
			return fmt.Errorf("load known dirs: %w", err)
		}
		for _, dir := range dirs {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				log.Printf("[%s] skipping desired dir %s: directory does not exist", m.profile.ID, dir)
				continue
			}
			m.sup.AddDesired(dir)
		}
	}
	go m.sup.Run(ctx)
	return nil
}

// CreateBackend builds the per-session backend. Guidance is assembled
// for fresh sessions only (mirrors the bespoke managers); skills
// materialize for both fresh and resumed sessions.
func (m *ACPBackendManager) CreateBackend(ctx context.Context, inv agent.BackendInvocation) (agent.SessionBackend, error) {
	guidanceText := ""
	if inv.ResumeExternalID == "" {
		guidanceText = guidance.Assemble(inv.WorkDir)
	}
	installGuidanceSkills(inv.WorkDir)

	resolver := func(ctx context.Context) (*acp.AdapterConn, error) {
		return m.sup.GetConn(ctx, inv.WorkDir)
	}
	return acp.NewBackend(m.profile, inv.WorkDir, inv.ResumeExternalID, guidanceText, resolver, log.Printf), nil
}

// configProbe is one in-flight on-demand config-options probe; waiters
// share its result.
type configProbe struct {
	done chan struct{}
	opts []agent.ConfigOption
	err  error
}

// ConfigOptions implements agent.ConfigOptionsLister: it opens one
// short-lived session in projectDir and returns the config options the
// agent advertises there — ACP surfaces them only on session open
// (zed-industries/zed#52500), so this is the only pre-session source.
// On-demand and uncached: callers (knob editors) show a spinner for
// exactly this call. Concurrent requests share one probe; the probe runs
// on its own context so one caller disconnecting doesn't fail the rest.
func (m *ACPBackendManager) ConfigOptions(ctx context.Context, projectDir string) ([]agent.ConfigOption, error) {
	key := projectDir
	if m.profile.Scope != acp.ScopePerDir {
		key = "" // host-scoped agents advertise the same options everywhere
	}
	m.probeMu.Lock()
	if m.closed {
		m.probeMu.Unlock()
		return nil, fmt.Errorf("acp %s: manager is shut down", m.profile.ID)
	}
	p, inFlight := m.probing[key]
	if !inFlight {
		p = &configProbe{done: make(chan struct{})}
		m.probing[key] = p
		m.probeWG.Add(1)
		go m.runConfigProbe(key, projectDir, p)
	}
	m.probeMu.Unlock()

	select {
	case <-p.done:
		return p.opts, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *ACPBackendManager) runConfigProbe(key, dir string, p *configProbe) {
	defer m.probeWG.Done()
	ctx, cancel := context.WithTimeout(context.Background(), configOptionsProbeTimeout)
	defer cancel()
	p.opts, p.err = m.probeConfigOptions(ctx, dir)
	if p.err != nil {
		log.Printf("[%s] config-options probe for %s: %v", m.profile.ID, dir, p.err)
	}
	m.probeMu.Lock()
	delete(m.probing, key)
	m.probeMu.Unlock()
	close(p.done)
}

// probeConfigOptions opens one session in dir and reads what it
// advertises. The session is stopped but not deleted: only
// claude-agent-acp supports session/delete, so an empty per-backend row
// is the accepted cost, matching every shipping ACP client.
func (m *ACPBackendManager) probeConfigOptions(ctx context.Context, dir string) ([]agent.ConfigOption, error) {
	b, err := m.CreateBackend(ctx, agent.BackendInvocation{WorkDir: dir})
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Stop() }()
	if err := b.Open(ctx); err != nil {
		return nil, err
	}
	r, ok := b.(agent.ConfigOptionsReporter)
	if !ok {
		return nil, nil
	}
	return r.ConfigOptions(), nil
}

// Shutdown stops every adapter process and waits for in-flight
// config-option probes so no goroutine outlives the manager.
func (m *ACPBackendManager) Shutdown() {
	m.probeMu.Lock()
	m.closed = true
	m.probeMu.Unlock()
	m.sup.StopAll()
	m.probeWG.Wait()
}

// DiscoverSessions lists the agent's own sessions for seedDir via ACP
// session/list, marking the dir desired for per-dir profiles.
func (m *ACPBackendManager) DiscoverSessions(ctx context.Context, seedDir string) ([]agent.SessionSnapshot, error) {
	conn, err := m.sup.GetConn(ctx, seedDir)
	if err != nil {
		return nil, err
	}
	lr, err := conn.Conn().ListSessions(ctx, sdk.ListSessionsRequest{Cwd: &seedDir})
	if err != nil {
		return nil, fmt.Errorf("acp %s: session/list: %w", m.profile.ID, err)
	}
	return m.snapshots(lr), nil
}

// DiscoverAllSessions lists every session the agent knows about. Only
// meaningful for host-scoped profiles (codex/claude adapters back it
// with their global stores); per-dir profiles return nothing — their
// discovery goes through per-dir seeds, matching the bespoke opencode
// exclusion.
func (m *ACPBackendManager) DiscoverAllSessions(ctx context.Context) ([]agent.SessionSnapshot, error) {
	if m.profile.Scope != acp.ScopeHost {
		return nil, nil
	}
	conn, err := m.sup.GetConn(ctx, "")
	if err != nil {
		return nil, err
	}
	lr, err := conn.Conn().ListSessions(ctx, sdk.ListSessionsRequest{})
	if err != nil {
		return nil, fmt.Errorf("acp %s: session/list: %w", m.profile.ID, err)
	}
	return m.snapshots(lr), nil
}

// snapshots maps ACP session infos onto clank snapshots, tagging the
// Backend (rehydration routes by it) and filtering ghost rows that have
// neither title nor timestamp (opencode #38064 class).
func (m *ACPBackendManager) snapshots(lr sdk.ListSessionsResponse) []agent.SessionSnapshot {
	out := make([]agent.SessionSnapshot, 0, len(lr.Sessions))
	for _, s := range lr.Sessions {
		title := ""
		if s.Title != nil {
			title = *s.Title
		}
		var updatedAt time.Time
		if s.UpdatedAt != nil {
			if ts, err := time.Parse(time.RFC3339Nano, *s.UpdatedAt); err == nil {
				updatedAt = ts
			}
		}
		if title == "" && updatedAt.IsZero() {
			continue
		}
		out = append(out, agent.SessionSnapshot{
			ID:        string(s.SessionId),
			Backend:   m.profile.Backend,
			Title:     title,
			Directory: s.Cwd,
			UpdatedAt: updatedAt,
		})
	}
	return out
}
