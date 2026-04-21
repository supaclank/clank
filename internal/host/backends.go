package host

// BackendManager implementations live on the Host plane. Each manages a
// specific backend type (OpenCode, Claude Code), owning any long-lived
// resources such as OpenCode server processes.

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/acksell/clank/internal/agent"
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
	return agent.NewOpenCodeBackend(serverURL, inv.ResumeExternalID, resolver), nil
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
			all = append(all, s)
		}
	}
	return all, nil
}

// ClaudeBackendManager implements agent.BackendManager for Claude Code sessions.
type ClaudeBackendManager struct{}

// NewClaudeBackendManager creates a new Claude backend manager.
func NewClaudeBackendManager() *ClaudeBackendManager {
	return &ClaudeBackendManager{}
}

// CreateBackend creates a Claude Code SessionBackend.
func (m *ClaudeBackendManager) CreateBackend(ctx context.Context, inv agent.BackendInvocation) (agent.SessionBackend, error) {
	return agent.NewClaudeCodeBackend(inv.WorkDir), nil
}

// Init is a no-op for Claude — there are no long-lived servers to manage.
func (m *ClaudeBackendManager) Init(ctx context.Context, knownDirs func() ([]string, error)) error {
	return nil
}

// Shutdown is a no-op for Claude — each session manages its own SDK client connection.
func (m *ClaudeBackendManager) Shutdown() {}
