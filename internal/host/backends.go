package host

// BackendManager implementations live on the Host plane. Each manages a
// specific backend type (OpenCode, Claude Code), owning any long-lived
// resources such as OpenCode server processes.

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/guidance"
	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// OpenCodeBackendManager implements agent.BackendManager, agent.AgentLister,
// agent.ModelLister, and agent.SessionDiscoverer for OpenCode sessions. It
// manages one OpenCode server per project directory via OpenCodeServerManager.
type OpenCodeBackendManager struct {
	serverMgr *agent.OpenCodeServerManager
}

// NewOpenCodeBackendManager creates a manager with a new server manager.
func NewOpenCodeBackendManager() *OpenCodeBackendManager {
	return &OpenCodeBackendManager{
		serverMgr: agent.NewOpenCodeServerManager(),
	}
}

// Init populates the desired server set from known project directories and
// starts the reconciler loop. The reconciler is the single owner of server
// lifecycle — it is the only code path that starts or stops servers.
// The first reconcile tick runs immediately, starting all known servers
// in parallel for fast startup.
func (m *OpenCodeBackendManager) Init(ctx context.Context, knownDirs func() ([]string, error)) error {
	dirs, err := knownDirs()
	if err != nil {
		return fmt.Errorf("load known dirs: %w", err)
	}
	var validDirs []string
	for _, dir := range dirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			log.Printf("[opencode] skipping desired dir %s: directory does not exist", dir)
			continue
		}
		validDirs = append(validDirs, dir)
	}
	if len(validDirs) > 0 {
		m.serverMgr.AddDesired(validDirs...)
		log.Printf("[opencode] added %d project dirs to desired set", len(validDirs))
	}

	go m.serverMgr.Run(ctx)
	return nil
}

// CreateBackend creates an OpenCode SessionBackend. It ensures an OpenCode
// server is running at inv.WorkDir before creating the backend.
// The backend receives a resolver closure that re-resolves the server URL
// on reconnect (handles server restarts on new ports).
func (m *OpenCodeBackendManager) CreateBackend(ctx context.Context, inv agent.BackendInvocation) (agent.SessionBackend, error) {
	serverURL, err := m.serverMgr.GetOrStartServer(ctx, inv.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("start opencode server for %s: %w", inv.WorkDir, err)
	}
	resolver := func(ctx context.Context) (string, error) {
		return m.serverMgr.GetOrStartServer(ctx, inv.WorkDir)
	}
	b := agent.NewOpenCodeBackend(serverURL, inv.ResumeExternalID, resolver)
	// Inject stack-detected guidance as the system prompt for fresh sessions
	// only; resumed sessions already carry it in conversation history.
	if inv.ResumeExternalID == "" {
		b.SystemPrompt = guidance.Assemble(inv.WorkDir)
	}
	installGuidanceSkills(inv.WorkDir)
	return b, nil
}

// installGuidanceSkills materializes the stack playbook the system prompt
// points at (guidance.InstallSkills) into ~/.claude/skills. Runs for fresh
// AND resumed sessions: the prompt already in a resumed session's history
// references the skill by path, and this refreshes stale copies after a
// clank upgrade. Non-fatal — a session without the playbook still has the
// distilled prompt. Fire-and-forget: the agent doesn't need the skill files
// at the exact millisecond CreateBackend returns, so this runs off the
// session-creation request path instead of blocking it.
func installGuidanceSkills(workDir string) {
	go func() {
		if err := guidance.InstallSkills(workDir); err != nil {
			log.Printf("guidance: install skills in %s: %v", workDir, err)
		}
	}()
}

// Shutdown stops all managed OpenCode servers.
func (m *OpenCodeBackendManager) Shutdown() {
	m.serverMgr.StopAll()
}

// ListAgents returns available agents for the given project directory.
func (m *OpenCodeBackendManager) ListAgents(ctx context.Context, projectDir string) ([]agent.AgentInfo, error) {
	return m.serverMgr.ListAgents(ctx, projectDir)
}

// ListModels returns available models from connected providers for the given project directory.
func (m *OpenCodeBackendManager) ListModels(ctx context.Context, projectDir string) ([]agent.ModelInfo, error) {
	return m.serverMgr.ListModels(ctx, projectDir)
}

// ServerManager returns the underlying server manager. Exported for tests
// that need to configure the manager (e.g. injecting a fake startServerFn).
func (m *OpenCodeBackendManager) ServerManager() *agent.OpenCodeServerManager {
	return m.serverMgr
}

// ListServers returns running OpenCode server info from the server manager.
func (m *OpenCodeBackendManager) ListServers() []agent.ServerInfo {
	return m.serverMgr.ListServers()
}

// DiscoverSessions returns every session known to opencode across every
// project worktree. opencode's HTTP /session API is project-scoped to the
// server's startup directory (even though the underlying SQLite DB is
// global), so we must hit one server per project. We use the seed server
// only to list the set of known projects, then iterate.
//
// Worktrees that are clearly invalid (root, empty, missing) are filtered
// out. Discovered worktrees are added to the desired set so the reconciler
// keeps servers running for future backend operations. Servers are started
// (or reused) serially via GetOrStartServer, which coalesces concurrent
// callers per dir.
//
// Sessions are deduped by ID in case opencode returns the same session
// from multiple servers (it shouldn't, but defensive).
func (m *OpenCodeBackendManager) DiscoverSessions(ctx context.Context, seedDir string) ([]agent.SessionSnapshot, error) {
	if _, err := m.serverMgr.GetOrStartServer(ctx, seedDir); err != nil {
		return nil, fmt.Errorf("get seed server: %w", err)
	}

	projects, err := m.serverMgr.ListProjects(ctx, seedDir)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	var validDirs []string
	for _, proj := range projects {
		if proj.Worktree == "" || proj.Worktree == "/" {
			continue
		}
		if _, err := os.Stat(proj.Worktree); os.IsNotExist(err) {
			continue
		}
		validDirs = append(validDirs, proj.Worktree)
	}

	if len(validDirs) > 0 {
		m.serverMgr.AddDesired(validDirs...)
	}

	seen := make(map[string]struct{})
	var all []agent.SessionSnapshot
	for _, dir := range validDirs {
		url, err := m.serverMgr.GetOrStartServer(ctx, dir)
		if err != nil {
			log.Printf("[opencode] discover: skipping %s: get server: %v", dir, err)
			continue
		}
		sessions, err := m.serverMgr.ListSessionsFromServer(ctx, url)
		if err != nil {
			log.Printf("[opencode] discover: skipping %s: list sessions: %v", dir, err)
			continue
		}
		for _, s := range sessions {
			if _, dup := seen[s.ID]; dup {
				continue
			}
			seen[s.ID] = struct{}{}
			// Tag the snapshot with its source backend so the hub can
			// persist info.Backend correctly. Without this, all discovered
			// sessions would be hardcoded to opencode at the registration
			// site, breaking reopen-after-restart for any other backend.
			s.Backend = agent.BackendOpenCode
			all = append(all, s)
		}
	}
	return all, nil
}

// CodexBackendManager implements agent.BackendManager for OpenAI Codex
// sessions. Each session spawns its own `codex app-server` subprocess (the
// Claude shape) — there are no shared servers to manage.
type CodexBackendManager struct{}

// NewCodexBackendManager creates a new codex backend manager.
func NewCodexBackendManager() *CodexBackendManager {
	return &CodexBackendManager{}
}

// CreateBackend creates a codex SessionBackend. When inv.ResumeExternalID is
// set, the backend reattaches to the existing codex thread via thread/resume
// at Open.
func (m *CodexBackendManager) CreateBackend(ctx context.Context, inv agent.BackendInvocation) (agent.SessionBackend, error) {
	b := agent.NewCodexBackendForSession(inv.WorkDir, inv.ResumeExternalID)
	// Same guidance the other backends inject, carried as codex
	// developerInstructions at thread start. Fresh sessions only — the
	// instructions persist in codex thread state across resume/fork.
	if inv.ResumeExternalID == "" {
		b.SystemPrompt = guidance.Assemble(inv.WorkDir)
	}
	installGuidanceSkills(inv.WorkDir)
	return b, nil
}

// Init is a no-op for codex — no long-lived servers to manage.
func (m *CodexBackendManager) Init(ctx context.Context, knownDirs func() ([]string, error)) error {
	return nil
}

// Shutdown is a no-op for codex — each session owns its subprocess.
func (m *CodexBackendManager) Shutdown() {}

// ClaudeBackendManager implements agent.BackendManager for Claude Code sessions.
type ClaudeBackendManager struct {
	// envResolver, when non-nil, is called once per CreateBackend to
	// build the env-var map injected into the spawned claude subprocess.
	// Service.New sets this to a closure that reads from AuthManager
	// (CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY). Resolved per-
	// session so reconnecting a provider mid-day takes effect on the
	// next new session without restarting the daemon.
	envResolver func() map[string]string
}

// NewClaudeBackendManager creates a new Claude backend manager. The
// env resolver is wired separately via SetEnvResolver after the
// AuthManager exists (chicken-and-egg: Service.New constructs the
// backend managers before the AuthManager).
func NewClaudeBackendManager() *ClaudeBackendManager {
	return &ClaudeBackendManager{}
}

// SetEnvResolver installs the closure used to populate ClaudeCodeBackend.ExtraEnv
// at session creation time. Pass nil to disable injection.
func (m *ClaudeBackendManager) SetEnvResolver(fn func() map[string]string) {
	m.envResolver = fn
}

// CreateBackend creates a Claude Code SessionBackend. When inv.ResumeExternalID
// is set (reopening a historical session), the backend is constructed with the
// session ID pre-seeded so Messages() can serve transcript history immediately,
// without needing Start to fire (activateBackend in the hub only calls Watch,
// which is a no-op for Claude — there is no long-lived process to attach to).
func (m *ClaudeBackendManager) CreateBackend(ctx context.Context, inv agent.BackendInvocation) (agent.SessionBackend, error) {
	b := agent.NewClaudeCodeBackendForSession(inv.WorkDir, inv.ResumeExternalID)
	if m.envResolver != nil {
		b.ExtraEnv = m.envResolver()
	}
	// Same guidance the OpenCode backend injects, applied via --append-system-prompt
	// at CLI launch for fresh sessions only.
	if inv.ResumeExternalID == "" {
		b.SystemPrompt = guidance.Assemble(inv.WorkDir)
	}
	installGuidanceSkills(inv.WorkDir)
	return b, nil
}

// ReadTranscript implements agent.TranscriptReader: it serves a Claude
// session's history straight from the on-disk JSONL transcript, without
// constructing a backend or spawning the claude CLI.
func (m *ClaudeBackendManager) ReadTranscript(ctx context.Context, workDir, externalID string) ([]agent.MessageData, error) {
	return agent.ReadClaudeTranscript(ctx, workDir, externalID)
}

// Init is a no-op for Claude — there are no long-lived servers to manage.
func (m *ClaudeBackendManager) Init(ctx context.Context, knownDirs func() ([]string, error)) error {
	return nil
}

// Shutdown is a no-op for Claude — each session manages its own SDK client connection.
func (m *ClaudeBackendManager) Shutdown() {}

// claudeModelCatalog enumerates the model families the claude CLI
// accepts via --model. These are AgentModel string constants
// (sonnet / opus / haiku from claude-agent-sdk-go, fable from
// internal/agent until the SDK ships it) — family aliases the CLI
// resolves to the latest version of each tier. We deliberately omit
// `inherit` because it's a "use whatever the caller's default is"
// passthrough, not a user-pickable model.
//
// Hardcoded rather than fetched from an Anthropic API because the
// model-list endpoint isn't part of claude-code's public surface —
// the SDK ships these as a closed enum. Bump the SDK to pick up new
// tiers (e.g. when Sonnet 4.5 lands a new family).
var claudeModelCatalog = []agent.ModelInfo{
	{
		ID:           string(claudecode.AgentModelSonnet),
		Name:         "Claude Sonnet",
		ProviderID:   ProviderAnthropicClaudeCode,
		ProviderName: "Anthropic",
	},
	{
		ID:           string(claudecode.AgentModelOpus),
		Name:         "Claude Opus",
		ProviderID:   ProviderAnthropicClaudeCode,
		ProviderName: "Anthropic",
	},
	{
		ID:           string(claudecode.AgentModelHaiku),
		Name:         "Claude Haiku",
		ProviderID:   ProviderAnthropicClaudeCode,
		ProviderName: "Anthropic",
	},
	{
		ID:           string(agent.AgentModelFable),
		Name:         "Claude Fable",
		ProviderID:   ProviderAnthropicClaudeCode,
		ProviderName: "Anthropic",
	},
}

// ListModels returns the model families supported by claude-code.
// Implements agent.ModelLister so the host service's ListModels
// dispatcher picks it up for /models?backend=claude-code requests.
// projectDir is ignored — claude's model list is global, not
// per-project (no plugins / no env-driven catalog like opencode).
func (m *ClaudeBackendManager) ListModels(_ context.Context, _ string) ([]agent.ModelInfo, error) {
	out := make([]agent.ModelInfo, len(claudeModelCatalog))
	copy(out, claudeModelCatalog)
	return out, nil
}

// DiscoverSessions returns historical Claude Code sessions visible to seedDir
// by reading the on-disk JSONL transcripts via the SDK's ListSessions. The
// SDK expands seedDir to include any git worktrees by default, mirroring how
// `claude --resume` resolves sessions across worktrees of the same repo.
//
// Sessions whose Cwd cannot be determined fall back to seedDir so the hub
// always has a directory to associate the session with.
func (m *ClaudeBackendManager) DiscoverSessions(ctx context.Context, seedDir string) ([]agent.SessionSnapshot, error) {
	if seedDir == "" {
		return nil, fmt.Errorf("claude discover: seedDir is required")
	}
	infos, err := claudecode.ListSessions(claudecode.WithSessionDirectory(seedDir))
	if err != nil {
		return nil, fmt.Errorf("list claude sessions for %s: %w", seedDir, err)
	}

	out := make([]agent.SessionSnapshot, 0, len(infos))
	for _, info := range infos {
		out = append(out, claudeSessionSnapshot(info, seedDir))
	}
	return out, nil
}

// DiscoverAllSessions enumerates every Claude Code session known to the SDK,
// across all projects under CLAUDE_CONFIG_DIR. Used by the hub's startup-
// discover pass to heal sessions whose persisted backend tag is wrong (and
// therefore whose GitRef.LocalPath cannot be trusted as a seed).
//
// Sessions whose Cwd is unset fall back to an empty Directory; the hub will
// preserve any existing GitRef.LocalPath on the persisted row in that case.
func (m *ClaudeBackendManager) DiscoverAllSessions(ctx context.Context) ([]agent.SessionSnapshot, error) {
	infos, err := claudecode.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("list all claude sessions: %w", err)
	}
	out := make([]agent.SessionSnapshot, 0, len(infos))
	for _, info := range infos {
		out = append(out, claudeSessionSnapshot(info, ""))
	}
	return out, nil
}

// claudeSessionSnapshot maps SDK metadata to the daemon's SessionSnapshot.
// The SDK already prioritizes custom_title > ai_title > first_prompt > timestamp
// when computing Summary, so we reuse it directly as Title.
func claudeSessionSnapshot(info claudecode.SDKSessionInfo, seedDir string) agent.SessionSnapshot {
	dir := seedDir
	if info.Cwd != nil && *info.Cwd != "" {
		dir = *info.Cwd
	}

	updated := time.UnixMilli(info.LastModified)
	created := updated
	if info.CreatedAt != nil {
		created = time.UnixMilli(*info.CreatedAt)
	}

	return agent.SessionSnapshot{
		ID:        info.SessionID,
		Backend:   agent.BackendClaudeCode,
		Title:     info.Summary,
		Directory: dir,
		CreatedAt: created,
		UpdatedAt: updated,
	}
}
