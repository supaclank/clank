package host

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acptools"
	"github.com/acksell/clank/internal/agent/guidance"
	sdk "github.com/coder/acp-go-sdk"
)

// ACPBackendManager adapts one acp.AdapterProfile to agent.BackendManager:
// a supervised adapter process pool plus per-session acp.Backend values.
// One generic implementation serves every ACP agent — per-adapter variance
// lives entirely in the profile.
type ACPBackendManager struct {
	profile acp.AdapterProfile
	sup     *acp.AdapterSupervisor
	envFn   atomic.Pointer[func() map[string]string]

	// catalog caches the agent-advertised model list per workDir. ACP
	// surfaces models as per-session config options, so the manager can
	// only answer /models from what a session has already reported —
	// it publishes on every session open (fresh and resumed).
	catalogMu sync.Mutex
	catalog   map[string][]agent.ModelInfo
	modes     map[string][]agent.SessionMode
	// probed marks dirs whose catalog we have already tried to fill, so a
	// dir whose agent advertises nothing isn't probed on every request.
	probed map[string]bool
	// probing single-flights concurrent probes per dir — /modes and
	// /models both miss on a cold dir and would otherwise each open a
	// session.
	probing map[string]chan struct{}
}

// NewACPBackendManager builds a manager for the given adapter profile.
// The profile's Env is routed through SetEnvResolver so credentials can
// be wired after construction (the AuthManager exists later) and rotated
// at runtime via the supervisor's env-fingerprint restarts.
func NewACPBackendManager(profile acp.AdapterProfile) (*ACPBackendManager, error) {
	m := &ACPBackendManager{}
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
func NewCodexACPManager(toolsDir string) (*ACPBackendManager, error) {
	var paths atomic.Pointer[acptools.Paths]
	profile := acp.CodexProfile("", "", nil)
	profile.Prepare = func(ctx context.Context, _ string) error {
		p, err := acptools.Ensure(ctx, toolsDir)
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
func NewClaudeACPManager(toolsDir string) (*ACPBackendManager, error) {
	var paths atomic.Pointer[acptools.Paths]
	profile := acp.ClaudeProfile("", "", nil)
	profile.Prepare = func(ctx context.Context, _ string) error {
		p, err := acptools.Ensure(ctx, toolsDir)
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
	b := acp.NewBackend(m.profile, inv.WorkDir, inv.ResumeExternalID, guidanceText, "", resolver, log.Printf)
	b.SetCatalogSink(m.putCatalog)
	b.SetModeSink(m.putModes)
	return b, nil
}

// putCatalog records a session's advertised models for its project dir.
func (m *ACPBackendManager) putCatalog(workDir string, models []agent.ModelInfo) {
	if len(models) == 0 {
		return
	}
	m.catalogMu.Lock()
	defer m.catalogMu.Unlock()
	if m.catalog == nil {
		m.catalog = make(map[string][]agent.ModelInfo)
	}
	// Clone so the catalog is independent of the producer's backing array.
	m.catalog[workDir] = slices.Clone(models)
}

// putModes records a session's advertised modes for its project dir.
func (m *ACPBackendManager) putModes(workDir string, modes []agent.SessionMode) {
	if len(modes) == 0 {
		return
	}
	m.catalogMu.Lock()
	defer m.catalogMu.Unlock()
	if m.modes == nil {
		m.modes = make(map[string][]agent.SessionMode)
	}
	// Clone so the catalog is independent of the producer's backing array.
	m.modes[workDir] = slices.Clone(modes)
}

// ListModes implements agent.ModeLister from what a session reported.
// Same caveat as ListModels: ACP advertises modes per session, so this
// is empty until one has opened, and any known list is a better answer
// than none for a dir we haven't seen.
func (m *ACPBackendManager) ListModes(ctx context.Context, projectDir string) ([]agent.SessionMode, error) {
	m.ensureCatalog(ctx, projectDir)
	m.catalogMu.Lock()
	defer m.catalogMu.Unlock()
	// Strictly per-dir: agents/modes are project-scoped (opencode reads
	// .opencode/agent/ from the repo), so answering for a dir we have not
	// seen with another dir's list would cross-contaminate projects.
	return slices.Clone(m.modes[projectDir]), nil
}

// ListModels implements agent.ModelLister from the per-dir catalog a
// session published on open. Empty before this host has opened a session
// in projectDir — ACP has no session-independent model listing, so the
// picker fills in once a session exists rather than showing a guess.
func (m *ACPBackendManager) ListModels(ctx context.Context, projectDir string) ([]agent.ModelInfo, error) {
	m.ensureCatalog(ctx, projectDir)
	m.catalogMu.Lock()
	defer m.catalogMu.Unlock()
	// Per-dir for the same reason as ListModes: project config can add or
	// restrict providers, so another dir's catalog is not a safe answer.
	return slices.Clone(m.catalog[projectDir]), nil
}

// ensureCatalog fills projectDir's mode/model catalog by opening a
// short-lived session, once per dir — ACP only advertises modes and
// models on the session/new|load|resume response, so a picker shown
// before the user's first session has to probe one to see them
// (zed-industries/zed#52500).
//
// The probe session is stopped but not deleted: only claude-agent-acp
// supports session/delete, so an empty per-backend row is the accepted
// cost of probing.
func (m *ACPBackendManager) ensureCatalog(ctx context.Context, projectDir string) {
	if projectDir == "" {
		return
	}
	m.catalogMu.Lock()
	if m.probed[projectDir] {
		m.catalogMu.Unlock()
		return
	}
	if wait, inFlight := m.probing[projectDir]; inFlight {
		m.catalogMu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		return
	}
	done := make(chan struct{})
	if m.probing == nil {
		m.probing = make(map[string]chan struct{})
	}
	m.probing[projectDir] = done
	m.catalogMu.Unlock()

	succeeded := false
	defer func() {
		m.catalogMu.Lock()
		if succeeded {
			if m.probed == nil {
				m.probed = make(map[string]bool)
			}
			m.probed[projectDir] = true
		}
		delete(m.probing, projectDir)
		m.catalogMu.Unlock()
		close(done)
	}()

	b, err := m.CreateBackend(ctx, agent.BackendInvocation{WorkDir: projectDir})
	if err != nil {
		log.Printf("[%s] catalog probe for %s: %v", m.profile.ID, projectDir, err)
		return
	}
	// Open publishes modes + models to the sinks CreateBackend wired.
	if err := b.Open(ctx); err != nil {
		log.Printf("[%s] catalog probe for %s: %v", m.profile.ID, projectDir, err)
		_ = b.Stop()
		return
	}
	succeeded = true
	_ = b.Stop()
}

// Shutdown stops every adapter process.
func (m *ACPBackendManager) Shutdown() { m.sup.StopAll() }

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
