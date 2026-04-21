package hub_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/hub"
)

// testRemoteURL is the canonical remote URL used by hub tests. The hub
// fixture pre-places a real git repo at the host's deterministic clone
// path (`<ClonesDir>/<CloneDirName(testRemoteURL)>/`) so any test that
// constructs `agent.GitRef{RemoteURL: testRemoteURL}`
// resolves on the host without needing a real network clone.
const testRemoteURL = "git@github.com:acksell/clank.git"

// registerTestRepo seeds a real git repo at the deterministic clone
// path for testRemoteURL on the hub's local host fixture, returning
// the absolute repo dir. The hub must already be running (i.e.
// startHubOnSocket has returned) so the host fixture has been built
// and its ClonesDir is known.
func registerTestRepo(t *testing.T, s *hub.Service) string {
	t.Helper()
	return registerTestRepoAtWithRef(t, s, agent.GitRef{RemoteURL: testRemoteURL})
}

// registerTestRepoAt is a back-compat alias for two-phase persistence
// tests. The dir parameter is intentionally ignored — the host is now
// path-free and resolves the testRemoteURL ref deterministically via
// CloneDirName under its own ClonesDir tempdir. The supplied dir from
// phase 1 is no longer reachable in phase 2 (each phase has its own
// host fixture with its own ClonesDir).
func registerTestRepoAt(t *testing.T, s *hub.Service, _ string) {
	t.Helper()
	registerTestRepo(t, s)
}

// registerTestRepoAtWithRef seeds a real git repo at the deterministic
// clone path for the given ref on the hub's local host fixture.
// Returns the seeded repo dir. Only Remote refs are supported (Local
// refs need no seeding because they resolve to the path the caller
// supplied directly).
func registerTestRepoAtWithRef(t *testing.T, s *hub.Service, ref agent.GitRef) string {
	t.Helper()
	if ref.RemoteURL == "" {
		t.Fatalf("registerTestRepoAtWithRef: expected Remote ref, got %+v", ref)
	}
	v, ok := hostFixturesByHub.Load(s)
	if !ok {
		t.Fatal("registerTestRepoAtWithRef: no host fixture for this hub.Service; ensure the test goes through testDaemon / ensureHostFixture")
	}
	f := v.(*hostTestFixture)
	name, err := agent.CloneDirName(ref.RemoteURL)
	if err != nil {
		t.Fatalf("CloneDirName: %v", err)
	}
	dir := filepath.Join(f.clonesDir, name)
	// Idempotent: if a prior call already seeded the repo (e.g. two
	// tests in the same package using the same testRemoteURL hit the
	// shared host fixture), reuse the existing repo rather than
	// re-running git init, which would clobber config and confuse
	// concurrent readers.
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil {
		return dir
	}
	// initGitRepoAt seeds a fresh repo with an `origin` remote so the
	// hub's discover path (git.RemoteURL on snap.Directory) recovers
	// the same Remote URL we keyed off of.
	initGitRepoAt(t, dir, ref.RemoteURL)
	return dir
}

// waitFor polls cond every 5ms until it returns true or timeout
// elapses. Used in place of fixed time.Sleep so tests don't pay the
// full sleep duration on fast machines and don't flake on slow ones.
// On timeout the test fails with msg + "timed out waiting" — callers
// should phrase msg as the condition that should have become true
// (e.g. "permission reply propagated to backend").
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
