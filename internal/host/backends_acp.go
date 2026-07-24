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

// catalogProbeTimeout bounds a background catalog probe: it opens one
// session and reads the advertised lists, so a few seconds is ample. Past
// it the adapter is unhealthy and the next read retries.
const catalogProbeTimeout = 30 * time.Second

// ACPBackendManager adapts one acp.AdapterProfile to agent.BackendManager:
// a supervised adapter process pool plus per-session acp.Backend values.
// One generic implementation serves every ACP agent — per-adapter variance
// lives entirely in the profile.
type ACPBackendManager struct {
	profile acp.AdapterProfile
	sup     *acp.AdapterSupervisor
	envFn   atomic.Pointer[func() map[string]string]

	// catalog caches the agent-advertised model/mode list per workDir. ACP
	// surfaces both only on the session/new|load|resume response, so the
	// manager answers /models and /modes from what a session reported and
	// publishes on every open (fresh and resumed). globalModels/globalModes
	// hold the backend-wide catalog from the neutral prewarm, served for any
	// dir a session hasn't specialized. All seeded from store and written
	// back through it.
	catalogMu    sync.Mutex
	catalog      map[string][]agent.ModelInfo
	modes        map[string][]agent.SessionMode
	globalModels []agent.ModelInfo
	globalModes  []agent.SessionMode
	// probed marks dirs whose catalog we have already tried to fill, so a
	// dir whose agent advertises nothing isn't probed on every request.
	// Persisted dirs start probed: their answer is already known.
	probed map[string]bool
	// probing single-flights concurrent probes per dir — /modes and
	// /models both miss on a cold dir and would otherwise each open a
	// session.
	probing map[string]chan struct{}
	// probeWG tracks background probe goroutines so Shutdown can wait them
	// out; closed stops new probes from starting during teardown.
	probeWG sync.WaitGroup
	closed  bool
	// globalProbing single-flights the neutral global probe that recovers a
	// host-scoped backend when the startup Prewarm failed (adapter warming).
	globalProbing bool
	// store persists the catalog so the maps above survive a restart.
	store *catalogStore
	// knownDirs, stashed from Init, are the project dirs Prewarm warms up
	// front for per-dir backends (the sandbox's repo, session-history dirs).
	knownDirs func() ([]string, error)
}

// ACPDirs locates the on-disk state an ACP manager owns.
type ACPDirs struct {
	// Tools holds the provisioned adapter runtime (bun plus the pinned
	// npm packages). Unused by backends that run the user's own binary.
	Tools string
	// Catalog holds the durable per-project mode/model catalog.
	Catalog string
}

// NewACPBackendManager builds a manager for the given adapter profile.
// The profile's Env is routed through SetEnvResolver so credentials can
// be wired after construction (the AuthManager exists later) and rotated
// at runtime via the supervisor's env-fingerprint restarts.
func NewACPBackendManager(profile acp.AdapterProfile, dirs ACPDirs) (*ACPBackendManager, error) {
	store, err := newCatalogStore(dirs.Catalog, profile.Backend)
	if err != nil {
		return nil, err
	}
	m := &ACPBackendManager{
		store:   store,
		catalog: map[string][]agent.ModelInfo{},
		modes:   map[string][]agent.SessionMode{},
		probed:  map[string]bool{},
		probing: map[string]chan struct{}{},
	}
	m.seed(store.snapshot())
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
	return NewACPBackendManager(profile, dirs)
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
	return NewACPBackendManager(profile, dirs)
}

// NewOpenCodeACPManager serves opencode through `opencode acp` on the
// user's own binary — their install, their state, no version skew clank
// can introduce. Prepare gates on the verified-surface floor (retried
// until it passes) and materializes stack-detected guidance as an
// instructions file inside the worktree's git dir; Env points opencode
// at it via inline config. Guidance is best-effort: it never blocks a
// session.
func NewOpenCodeACPManager(dirs ACPDirs) (*ACPBackendManager, error) {
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
	return NewACPBackendManager(profile, dirs)
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
	m.knownDirs = knownDirs
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
// Wired as the model sink on every backend, so a real session open heals a
// stale persisted catalog for free.
func (m *ACPBackendManager) putCatalog(workDir string, models []agent.ModelInfo) {
	m.storeDirModels(workDir, models)
}

// putModes records a session's advertised modes for its project dir.
func (m *ACPBackendManager) putModes(workDir string, modes []agent.SessionMode) {
	m.storeDirModes(workDir, modes)
}

func (m *ACPBackendManager) storeDirModels(workDir string, models []agent.ModelInfo) {
	if len(models) == 0 || workDir == "" {
		return
	}
	m.catalogMu.Lock()
	// Clone so the catalog is independent of the producer's backing array.
	m.catalog[workDir] = slices.Clone(models)
	// A real session filled this dir — no background probe needed for it.
	m.probed[workDir] = true
	m.catalogMu.Unlock()
	// Write through outside catalogMu (store takes its own lock).
	m.store.putDir(workDir, func(e *catalogEntry) { e.Models = slices.Clone(models) })
}

func (m *ACPBackendManager) storeDirModes(workDir string, modes []agent.SessionMode) {
	if len(modes) == 0 || workDir == "" {
		return
	}
	m.catalogMu.Lock()
	m.modes[workDir] = slices.Clone(modes)
	m.probed[workDir] = true
	m.catalogMu.Unlock()
	m.store.putDir(workDir, func(e *catalogEntry) { e.Modes = slices.Clone(modes) })
}

func (m *ACPBackendManager) storeGlobal(models []agent.ModelInfo, modes []agent.SessionMode) {
	m.catalogMu.Lock()
	if len(models) > 0 {
		m.globalModels = slices.Clone(models)
	}
	if len(modes) > 0 {
		m.globalModes = slices.Clone(modes)
	}
	m.catalogMu.Unlock()
	m.store.putGlobal(func(e *catalogEntry) {
		if len(models) > 0 {
			e.Models = slices.Clone(models)
		}
		if len(modes) > 0 {
			e.Modes = slices.Clone(modes)
		}
	})
}

// ListModes implements agent.ModeLister. It never blocks: it serves the
// dir's catalog if a session has specialized it, else the backend-global
// catalog from prewarm. A per-dir backend on a dir it hasn't seen kicks a
// background probe so the specialized list is ready on the next read.
func (m *ACPBackendManager) ListModes(_ context.Context, projectDir string) ([]agent.SessionMode, error) {
	m.ensureCatalogAsync(projectDir)
	m.catalogMu.Lock()
	defer m.catalogMu.Unlock()
	if v, ok := m.modes[projectDir]; ok {
		return slices.Clone(v), nil
	}
	return slices.Clone(m.globalModes), nil
}

// ListModels implements agent.ModelLister with the same serve-then-refine
// contract as ListModes.
func (m *ACPBackendManager) ListModels(_ context.Context, projectDir string) ([]agent.ModelInfo, error) {
	m.ensureCatalogAsync(projectDir)
	m.catalogMu.Lock()
	defer m.catalogMu.Unlock()
	if v, ok := m.catalog[projectDir]; ok {
		return slices.Clone(v), nil
	}
	return slices.Clone(m.globalModels), nil
}

// ensureCatalogAsync kicks whatever background probe a cold read needs: a
// per-dir backend probes the specific repo; a host-scoped backend re-probes
// the neutral global catalog if the startup Prewarm never filled it. Both
// are non-blocking and single-flighted.
func (m *ACPBackendManager) ensureCatalogAsync(projectDir string) {
	if m.profile.Scope == acp.ScopePerDir {
		m.probeDirInBackground(projectDir)
		return
	}
	m.probeGlobalInBackground()
}

// probeGlobalInBackground refills the backend-global catalog for a
// host-scoped backend whose Prewarm hasn't succeeded yet. No-op once the
// catalog is non-empty or a probe is already in flight.
func (m *ACPBackendManager) probeGlobalInBackground() {
	m.catalogMu.Lock()
	if m.closed || m.globalProbing || len(m.globalModels) > 0 || len(m.globalModes) > 0 {
		m.catalogMu.Unlock()
		return
	}
	m.globalProbing = true
	m.probeWG.Add(1)
	m.catalogMu.Unlock()

	go func() {
		defer m.probeWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), catalogProbeTimeout)
		defer cancel()
		if neutral, err := os.MkdirTemp("", "clank-catalog-probe-"); err == nil {
			defer os.RemoveAll(neutral)
			m.probe(ctx, neutral, true)
		} else {
			log.Printf("[%s] catalog re-probe: temp dir: %v", m.profile.ID, err)
		}
		m.catalogMu.Lock()
		m.globalProbing = false
		m.catalogMu.Unlock()
	}()
}

// probeDirInBackground opens one short-lived session for a per-dir backend
// on a dir it hasn't specialized yet, so opencode's per-repo agents reach
// the picker. Host-scoped backends (claude, codex) have no per-dir variance
// — their prewarmed global catalog is authoritative, so this is a no-op and
// no dead per-folder session is ever created for them.
//
// Fire-and-forget: /modes and /models miss together on a cold dir but open
// only one session, single-flighted via probeOnceForDir against any other
// caller — including Prewarm's own per-dir sweep. The probe runs on its own
// context, not the request's — the HTTP read returns immediately, and
// cancelling it must not abort a probe another caller is waiting on.
func (m *ACPBackendManager) probeDirInBackground(projectDir string) {
	if projectDir == "" || m.profile.Scope != acp.ScopePerDir {
		return
	}
	m.catalogMu.Lock()
	skip := m.closed || m.probed[projectDir]
	if !skip {
		_, skip = m.probing[projectDir]
	}
	if !skip {
		m.probeWG.Add(1)
	}
	m.catalogMu.Unlock()
	if skip {
		return
	}

	go func() {
		defer m.probeWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), catalogProbeTimeout)
		defer cancel()
		m.probeOnceForDir(ctx, projectDir)
	}()
}

// probeOnceForDir single-flights probe() for dir against every caller —
// probeDirInBackground and Prewarm's per-dir sweep alike — so a dir being
// probed by one never gets a second, redundant session opened by the other.
// Returns false without probing if dir is already probed, already in
// flight, or the manager is shutting down.
func (m *ACPBackendManager) probeOnceForDir(ctx context.Context, dir string) bool {
	m.catalogMu.Lock()
	if m.closed || m.probed[dir] {
		m.catalogMu.Unlock()
		return false
	}
	if _, inFlight := m.probing[dir]; inFlight {
		m.catalogMu.Unlock()
		return false
	}
	done := make(chan struct{})
	m.probing[dir] = done
	m.catalogMu.Unlock()

	ok := m.probe(ctx, dir, false)

	m.catalogMu.Lock()
	if ok {
		m.probed[dir] = true
	}
	delete(m.probing, dir)
	m.catalogMu.Unlock()
	close(done)
	return ok
}

// probe opens one session in dir, reads the catalog it advertises, and
// stores it globally or per-dir. Returns whether the session opened — a
// transient failure (adapter warming up, timeout) must not mark the dir
// probed, so the next caller retries. ACP advertises modes and models only
// on session open, so this is the only way to fill a picker before the
// user's first prompt (zed-industries/zed#52500).
//
// The session is stopped but not deleted: only claude-agent-acp supports
// session/delete, so an empty per-backend row is the accepted cost, matching
// every shipping ACP client.
func (m *ACPBackendManager) probe(ctx context.Context, dir string, global bool) bool {
	b, err := m.CreateBackend(ctx, agent.BackendInvocation{WorkDir: dir})
	if err != nil {
		log.Printf("[%s] catalog probe for %s: %v", m.profile.ID, dir, err)
		return false
	}
	defer func() { _ = b.Stop() }()
	if global {
		// dir is a throwaway temp path removed right after this call returns;
		// clear the per-dir sinks so Open doesn't persist a dead entry for it.
		if nb, ok := b.(*acp.Backend); ok {
			nb.SetCatalogSink(nil)
			nb.SetModeSink(nil)
		}
	}
	if err := b.Open(ctx); err != nil {
		log.Printf("[%s] catalog probe for %s: %v", m.profile.ID, dir, err)
		return false
	}
	// The sinks fired per-dir during Open; for the global prewarm read the
	// advertised catalog directly and store it under the global scope.
	if global {
		var models []agent.ModelInfo
		var modes []agent.SessionMode
		if r, ok := b.(agent.ModelReporter); ok {
			_, models = r.Models()
		}
		if r, ok := b.(agent.ModeReporter); ok {
			_, modes = r.Modes()
		}
		m.storeGlobal(models, modes)
	}
	return true
}

// Prewarm fills the catalog before the user reaches a picker: one neutral
// probe for the backend-global catalog (so any dir answers instantly), plus
// each known dir for per-dir backends (so a sandbox's repo or a laptop's
// session-history repos have their agents ready too). Best-effort and
// invisible — it runs in the background at host start and never blocks
// readiness. Re-running is safe: it heals a stale or lost catalog.
//
// Registers with probeWG like every other probe path, so Shutdown — called
// from PrewarmCatalogs's untracked `go pw.Prewarm(ctx)` — still waits it out
// instead of returning while Prewarm is mid-probe.
func (m *ACPBackendManager) Prewarm(ctx context.Context) {
	m.catalogMu.Lock()
	if m.closed {
		m.catalogMu.Unlock()
		return
	}
	m.probeWG.Add(1)
	m.catalogMu.Unlock()
	defer m.probeWG.Done()

	neutral, err := os.MkdirTemp("", "clank-catalog-probe-")
	if err != nil {
		log.Printf("[%s] prewarm: temp dir: %v", m.profile.ID, err)
	} else {
		defer os.RemoveAll(neutral)
		m.probe(ctx, neutral, true)
	}

	if m.profile.Scope != acp.ScopePerDir || m.knownDirs == nil {
		return
	}
	dirs, err := m.knownDirs()
	if err != nil {
		log.Printf("[%s] prewarm: known dirs: %v", m.profile.ID, err)
		return
	}
	for _, dir := range dirs {
		if ctx.Err() != nil {
			return
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			continue
		}
		m.probeOnceForDir(ctx, dir)
	}
}

// Shutdown stops every adapter process and waits for in-flight catalog
// probes so no goroutine outlives the manager.
func (m *ACPBackendManager) Shutdown() {
	m.catalogMu.Lock()
	m.closed = true
	m.catalogMu.Unlock()
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
