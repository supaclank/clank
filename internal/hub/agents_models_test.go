package hub_test

import (
	"context"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/hub"
	"github.com/acksell/clank/internal/store"
)

// testRef is the GitRef used in catalog tests. The host doesn't have a
// real repo registered for this ref — but the host's catalog endpoint
// only invokes the BackendManager's lister with the (ref→workdir)
// resolution result, and our mock listers ignore the workdir. The host
// still resolves the ref to a workdir via repoRoot, which fails when the
// ref is unknown. Tests that exercise the wire path therefore register a
// repo on the local host fixture before calling. (Below we use the "no
// repo" ref directly — the lister returns its canned response without
// touching workdir, but the host's catalog implementation actually walks
// through repoRoot first. We register repos via the host fixture helper.)
//
// Keeping this as a package-level fixture avoids re-deriving it in every
// test and documents the intent: catalog identity = (backend, host, ref),
// branch deliberately omitted.
var testRef = agent.GitRef{RemoteURL: "https://example.com/test.git"}

func TestDaemonListAgents(t *testing.T) {
	t.Parallel()

	s := hub.New()

	// OpenCode manager with agent listing support.
	ocMgr := &mockAgentListerManager{
		agents: func(ctx context.Context, projectDir string) ([]agent.AgentInfo, error) {
			return []agent.AgentInfo{
				{Name: "build", Description: "Build agent", Mode: "primary"},
				{Name: "plan", Description: "Plan agent", Mode: "primary"},
			}, nil
		},
	}
	s.BackendManagers[agent.BackendOpenCode] = ocMgr

	// Claude manager — no agent lister support.
	s.BackendManagers[agent.BackendClaudeCode] = newMockBackendManager()

	client, _, cleanup := startHubOnSocket(t, s)
	defer cleanup()

	// The host's catalog handler resolves ref→workdir via repoRoot, so
	// we must register the repo on the local host fixture first.
	registerTestRepoAtWithRef(t, s, testRef)

	ctx := context.Background()

	// List agents for OpenCode backend.
	agents, err := client.Backend(agent.BackendOpenCode).Agents(ctx, host.HostLocal, testRef)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
	if agents[0].Name != "build" || agents[1].Name != "plan" {
		t.Errorf("unexpected agents: %+v", agents)
	}

	// List agents for Claude Code (no agent lister support).
	agents, err = client.Backend(agent.BackendClaudeCode).Agents(ctx, host.HostLocal, testRef)
	if err != nil {
		t.Fatalf("ListAgents for Claude Code: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents for Claude Code, got %d", len(agents))
	}
}

func TestDaemonListAgentsMissingParams(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	// Missing backend param — should return an error.
	_, err := client.Backend("").Agents(ctx, host.HostLocal, testRef)
	if err == nil {
		t.Error("expected error for missing backend param")
	}
}

func TestDaemonListAgentsReturnsCachedFromStore(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	dbPath := dir + "/test.db"

	// Pre-seed the store with cached primary agents.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cachedAgents := []agent.AgentInfo{
		{Name: "build", Description: "Cached build", Mode: "primary"},
		{Name: "plan", Description: "Cached plan", Mode: "primary"},
	}
	if err := st.UpsertPrimaryAgents(agent.BackendOpenCode, string(host.HostLocal), testRef, cachedAgents); err != nil {
		t.Fatalf("UpsertPrimaryAgents: %v", err)
	}

	s := hub.New()
	s.Store = st

	// Use an agent lister that tracks whether it was called synchronously.
	// The lister blocks until explicitly unblocked, so if the handler
	// returns before unblocking, we know it served from cache.
	listerCalled := make(chan struct{}, 1)
	ocMgr := &mockAgentListerManager{
		agents: func(ctx context.Context, projectDir string) ([]agent.AgentInfo, error) {
			listerCalled <- struct{}{}
			return []agent.AgentInfo{
				{Name: "build", Description: "Fresh build", Mode: "primary"},
				{Name: "plan", Description: "Fresh plan", Mode: "primary"},
				{Name: "debug", Description: "Fresh debug", Mode: "primary"},
			}, nil
		},
	}
	s.BackendManagers[agent.BackendOpenCode] = ocMgr
	s.BackendManagers[agent.BackendClaudeCode] = newMockBackendManager()

	client, _, cleanup := startHubOnSocket(t, s)
	defer cleanup()

	registerTestRepoAtWithRef(t, s, testRef)

	ctx := context.Background()

	// Request agents — should return cached data immediately.
	agents, err := client.Backend(agent.BackendOpenCode).Agents(ctx, host.HostLocal, testRef)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	// Should get the CACHED agents (2 agents, not 3).
	if len(agents) != 2 {
		t.Fatalf("expected 2 cached agents, got %d: %+v", len(agents), agents)
	}
	if agents[0].Description != "Cached build" {
		t.Errorf("expected cached description, got %q", agents[0].Description)
	}

	// The background refresh should have been triggered.
	select {
	case <-listerCalled:
		// Good — background refresh happened.
	case <-time.After(5 * time.Second):
		t.Error("expected background refresh to be triggered")
	}

	// After the refresh completes, subsequent requests should get the
	// fresh data. Poll rather than sleep — the refresh runs on a
	// goroutine and finishes well under 200ms in practice, but a
	// congested CI can make the fixed sleep flaky either way.
	waitFor(t, 2*time.Second, "cache refreshed to 3 agents", func() bool {
		got, err := client.Backend(agent.BackendOpenCode).Agents(ctx, host.HostLocal, testRef)
		return err == nil && len(got) == 3
	})

	agents, err = client.Backend(agent.BackendOpenCode).Agents(ctx, host.HostLocal, testRef)
	if err != nil {
		t.Fatalf("ListAgents (2nd call): %v", err)
	}
	if len(agents) != 3 {
		t.Fatalf("expected 3 fresh agents after refresh, got %d: %+v", len(agents), agents)
	}
	if agents[0].Description != "Fresh build" {
		t.Errorf("expected fresh description, got %q", agents[0].Description)
	}
}

func TestDaemonListAgentsFallsBackToListerOnCacheMiss(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	dbPath := dir + "/test.db"

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// No pre-seeded agents — cache miss.

	s := hub.New()
	s.Store = st

	ocMgr := &mockAgentListerManager{
		agents: func(ctx context.Context, projectDir string) ([]agent.AgentInfo, error) {
			return []agent.AgentInfo{
				{Name: "build", Description: "Build agent", Mode: "primary"},
			}, nil
		},
	}
	s.BackendManagers[agent.BackendOpenCode] = ocMgr
	s.BackendManagers[agent.BackendClaudeCode] = newMockBackendManager()

	client, _, cleanup := startHubOnSocket(t, s)
	defer cleanup()

	registerTestRepoAtWithRef(t, s, testRef)

	ctx := context.Background()

	// No cache — should fall back to synchronous lister call.
	agents, err := client.Backend(agent.BackendOpenCode).Agents(ctx, host.HostLocal, testRef)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "build" {
		t.Errorf("unexpected agents: %+v", agents)
	}

	// After the synchronous call, the result should be persisted.
	cached, err := st.LoadPrimaryAgents(agent.BackendOpenCode, string(host.HostLocal), testRef)
	if err != nil {
		t.Fatalf("LoadPrimaryAgents: %v", err)
	}
	if len(cached) != 1 || cached[0].Name != "build" {
		t.Errorf("expected persisted agents, got %+v", cached)
	}
}
