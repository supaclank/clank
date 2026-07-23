package host

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/acp"
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
}

// NewACPBackendManager builds a manager for the given adapter profile.
func NewACPBackendManager(profile acp.AdapterProfile) (*ACPBackendManager, error) {
	sup, err := acp.NewAdapterSupervisor(profile, log.Printf)
	if err != nil {
		return nil, err
	}
	return &ACPBackendManager{profile: profile, sup: sup}, nil
}

// NewCodexACPManager builds the codex manager: codex-acp under bun, env
// from the OpenAI sink (nil env = codex's own ChatGPT login fallback).
func NewCodexACPManager(bunBin, adapterEntry string, env func() map[string]string) (*ACPBackendManager, error) {
	return NewACPBackendManager(acp.CodexProfile(bunBin, adapterEntry, env))
}

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
	return acp.NewBackend(m.profile, inv.WorkDir, inv.ResumeExternalID, guidanceText, "", resolver, log.Printf), nil
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
