package host

// BackendManager implementations live on the Host plane. Each manages a
// specific backend type (OpenCode, Claude Code), owning any long-lived
// resources such as OpenCode server processes.

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/guidance"
	claudecode "github.com/severity1/claude-agent-sdk-go"
)

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
