//go:build unix

package preview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixtureExpoWorkDir creates a minimal worktree that Detect classifies
// as Expo. Used by the manager tests so Manager.Start has something to
// detect without us bringing in real node_modules.
func fixtureExpoWorkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "package.json"), `{"dependencies":{"expo":"~50.0.0"}}`)
	mustWrite(t, filepath.Join(dir, "app.json"), `{"expo":{"name":"fixture"}}`)
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// testSpec returns a Spec tests can pass straight into
// Manager.startWithSpec, bypassing Detect (and therefore bypassing the
// package-level expoCmdTemplate that previously raced under -race).
func testSpec(argv []string) Spec {
	return Spec{
		Kind:        KindExpo,
		CmdTemplate: argv,
		ReadyProbe:  expoReadyProbe,
	}
}

// TestManagerStartIdempotent confirms a second Start for the same
// worktree returns the existing snapshot instead of spawning twice.
// Without idempotency, a mobile retry on a slow network would leak
// dev-server processes.
func TestManagerStartIdempotent(t *testing.T) {
	t.Parallel()
	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()

	dir := fixtureExpoWorkDir(t)
	wid := "wt-idempotent"
	spec := testSpec(fakeMetroScript(""))

	first, err := m.startWithSpec(context.Background(), wid, dir, "default", spec, 0)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if first.Port == 0 {
		t.Fatalf("first start: port not assigned")
	}

	second, err := m.startWithSpec(context.Background(), wid, dir, "default", spec, 0)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if second.Port != first.Port {
		t.Fatalf("second start spawned a different server: ports %d vs %d", first.Port, second.Port)
	}
}

// TestManagerStartRespawnsAfterFailure pins the stale-entry eviction
// in Start. Without it, a crashed Metro leaves a Failed snapshot in
// the map and every subsequent Start returns that snapshot — the user
// is stuck until they explicitly /stop. Regression for cubic#2.
//
// Start is non-blocking: it returns a StateStarting snapshot immediately
// and the background probe settles the record to Failed/Ready, so we poll
// the state rather than reading it off the Start return. What this pins:
// a failed start must not leak state that blocks a retry.
func TestManagerStartRespawnsAfterFailure(t *testing.T) {
	t.Parallel()
	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()

	dir := fixtureExpoWorkDir(t)
	wid := "wt-respawn"
	key := serviceKey{WorktreeID: wid, ServiceName: "default"}
	getRunning := func() *running {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.servers[key]
	}

	// First start: script never serves /status, so the probe times out and
	// the background goroutine flips the record to Failed. The start call
	// itself returns immediately with StateStarting (non-blocking).
	deadSpec := testSpec([]string{"sh", "-c", "sleep 30"})
	s1, err := m.startWithSpec(context.Background(), wid, dir, "default", deadSpec, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("first start: unexpected error: %v", err)
	}
	if s1.State != StateStarting {
		t.Fatalf("first start state = %s, want starting (non-blocking)", s1.State)
	}
	r1 := getRunning()
	if r1 == nil {
		t.Fatalf("first start: no record published")
	}
	waitForState(t, r1, StateFailed, 3*time.Second)

	// Second start with a healthy stub: must evict the Failed entry, spawn
	// fresh, and reach Ready in the background.
	liveSpec := testSpec(fakeMetroScript(""))
	s2, err := m.startWithSpec(context.Background(), wid, dir, "default", liveSpec, 0)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if s2.State != StateStarting {
		t.Fatalf("second start state = %s, want starting", s2.State)
	}
	r2 := getRunning()
	if r2 == nil {
		t.Fatalf("second start: no record published")
	}
	waitForState(t, r2, StateReady, 5*time.Second)
}

// TestManagerStartNotPreviewable confirms Start fast-fails on a
// non-Expo worktree without leaving anything behind.
func TestManagerStartNotPreviewable(t *testing.T) {
	t.Parallel()
	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()

	plain := t.TempDir() // no package.json
	_, err := m.Start(context.Background(), "wt-empty", plain, "http://localhost:8080/preview/test")
	if !errors.Is(err, ErrNotPreviewable) {
		t.Fatalf("Start on non-Expo dir: got %v, want ErrNotPreviewable", err)
	}
}

// TestManagerStopErrNotRunning confirms Stop returns ErrNotRunning
// when no server exists. Idempotent stop is what lets the mobile
// "close preview" handler not care whether anything's actually running.
func TestManagerStopErrNotRunning(t *testing.T) {
	t.Parallel()
	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()
	if err := m.Stop("nonexistent"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop on missing worktree: got %v, want ErrNotRunning", err)
	}
}

// TestManagerStatusReflectsAvailability confirms Status returns
// available=true for an Expo dir even when no server is running, and
// available=false for a non-Expo dir.
func TestManagerStatusReflectsAvailability(t *testing.T) {
	t.Parallel()
	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()

	expoDir := fixtureExpoWorkDir(t)
	plainDir := t.TempDir()

	s, err := m.Status(context.Background(), "any", expoDir)
	if err != nil {
		t.Fatalf("Status on expo dir: %v", err)
	}
	if !s.Available || s.State != StateStopped {
		t.Errorf("expo dir status = %+v; want Available=true, State=stopped", s)
	}
	if s.Kind != KindExpo {
		t.Errorf("expo dir Kind = %q; want %q", s.Kind, KindExpo)
	}

	s2, err := m.Status(context.Background(), "any", plainDir)
	if err != nil {
		t.Fatalf("Status on plain dir: %v", err)
	}
	if s2.Available || s2.State != StateStopped {
		t.Errorf("plain dir status = %+v; want Available=false, State=stopped", s2)
	}
}

// TestManagerShutdownStopsAll spawns two stubs and confirms Shutdown
// reaps both process trees (Change 2 again, at the manager level).
func TestManagerShutdownStopsAll(t *testing.T) {
	t.Parallel()
	spec := testSpec(fakeMetroScript("(sleep 30 &)"))

	m := New(Options{StopGrace: 2 * time.Second})

	dir := fixtureExpoWorkDir(t)
	_, err := m.startWithSpec(context.Background(), "wt-1", dir, "default", spec, 0)
	if err != nil {
		t.Fatalf("start wt-1: %v", err)
	}
	_, err = m.startWithSpec(context.Background(), "wt-2", dir, "default", spec, 0)
	if err != nil {
		t.Fatalf("start wt-2: %v", err)
	}

	// Collect their pgids for the post-shutdown assertion.
	m.mu.Lock()
	pgids := make([]int, 0, len(m.servers))
	for _, r := range m.servers {
		pgids = append(pgids, r.pgid)
	}
	m.mu.Unlock()
	if len(pgids) != 2 {
		t.Fatalf("expected 2 running servers, got %d", len(pgids))
	}

	m.Shutdown()

	// Re-Shutdown should be idempotent.
	m.Shutdown()

	for _, pgid := range pgids {
		if got := waitForGroupEmpty(t, pgid, 2*time.Second); got != 0 {
			t.Errorf("group %d not empty after Shutdown (got %d)", pgid, got)
		}
	}
}
