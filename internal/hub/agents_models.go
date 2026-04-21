package hub

import (
	"context"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// HUB
// catalogKey produces a stable map key for the primary-agent in-flight
// refresh dedup table. The catalog is keyed on (backend, hostname, repo)
// per §7.8 — branches deliberately share the same catalog because
// opencode/claude config is committed to git.
func catalogKey(bt agent.BackendType, hostname host.Hostname, ref agent.GitRef) string {
	// Drop the branch from the cache key — same repo, same catalog
	// regardless of which worktree is checked out.
	bareRef := ref
	bareRef.WorktreeBranch = ""
	return string(bt) + "\x00" + string(hostname) + "\x00" + agent.RepoKey(bareRef)
}

// HUB
// persistPrimaryAgents writes the primary agent list to the store for future cache hits.
func (s *Service) persistPrimaryAgents(bt agent.BackendType, hostname host.Hostname, ref agent.GitRef, agents []agent.AgentInfo) {
	if s.Store == nil {
		return
	}
	if err := s.Store.UpsertPrimaryAgents(bt, string(hostname), ref, agents); err != nil {
		s.log.Printf("warning: persist primary agents for %s/%s/%s: %v", bt, hostname, agent.RepoKey(ref), err)
	}
}

// HUB
// refreshPrimaryAgentsInBackground kicks off an async refresh of the
// primary agent list for the given (backend, host, repo). The result is
// persisted to SQLite so that subsequent requests get the updated list.
// Safe to call multiple times — concurrent refreshes for the same key
// are deduplicated.
func (s *Service) refreshPrimaryAgentsInBackground(bt agent.BackendType, hostname host.Hostname, ref agent.GitRef) {
	key := catalogKey(bt, hostname, ref)
	s.primaryAgentsRefreshMu.Lock()
	if s.primaryAgentsRefreshInFlight[key] {
		s.primaryAgentsRefreshMu.Unlock()
		return
	}
	s.primaryAgentsRefreshInFlight[key] = true
	s.primaryAgentsRefreshMu.Unlock()

	go func() {
		defer func() {
			s.primaryAgentsRefreshMu.Lock()
			delete(s.primaryAgentsRefreshInFlight, key)
			s.primaryAgentsRefreshMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		defer cancel()

		hc, ok := s.Host(hostname)
		if !ok {
			s.log.Printf("background primary agent refresh: host %q not registered", hostname)
			return
		}
		agents, err := hc.Backend(bt).Agents(ctx, ref)
		if err != nil {
			s.log.Printf("background primary agent refresh for %s/%s/%s: %v", bt, hostname, agent.RepoKey(ref), err)
			return
		}
		if agents == nil {
			agents = []agent.AgentInfo{}
		}
		s.persistPrimaryAgents(bt, hostname, ref, agents)
	}()
}

// HUB
// warmPrimaryAgentCaches fetches and persists primary agent lists for
// every (backend, host, repo) target derived from the sessions table.
// Called once on daemon startup after the reconciler has been started.
// Targets whose host is not currently registered are skipped — the
// catalog will warm next time that host registers (best-effort).
func (s *Service) warmPrimaryAgentCaches() {
	if s.Store == nil {
		return
	}
	targets, err := s.Store.KnownAgentTargets()
	if err != nil {
		s.log.Printf("warning: load known agent targets: %v", err)
		return
	}
	for _, t := range targets {
		hostname := host.Hostname(t.Hostname)
		if _, ok := s.Host(hostname); !ok {
			continue
		}
		s.refreshPrimaryAgentsInBackground(t.Backend, hostname, t.GitRef)
	}
}
