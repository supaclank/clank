//go:build unix

package preview

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

	first, err := m.startWithSpec(context.Background(), wid, dir, "http://localhost:8080/preview/test", spec, 0)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if first.Port == 0 {
		t.Fatalf("first start: port not assigned")
	}

	second, err := m.startWithSpec(context.Background(), wid, dir, "http://localhost:8080/preview/test", spec, 0)
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
func TestManagerStartRespawnsAfterFailure(t *testing.T) {
	t.Parallel()
	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()

	dir := fixtureExpoWorkDir(t)
	wid := "wt-respawn"

	// First start: script never serves /status, probe times out fast,
	// state ends as Failed.
	deadSpec := testSpec([]string{"sh", "-c", "sleep 30"})
	first, err := m.startWithSpec(context.Background(), wid, dir, "http://localhost:8080/preview/wt-respawn", deadSpec, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if first.State != StateStarting {
		t.Fatalf("first start: want StateStarting (probe still running), got %s", first.State)
	}
	waitForStateVia(t, m, wid, dir, StateFailed, 2*time.Second)

	// Second start with a healthy stub: must spawn fresh, not return
	// the cached Failed snapshot.
	liveSpec := testSpec(fakeMetroScript(""))
	second, err := m.startWithSpec(context.Background(), wid, dir, "http://localhost:8080/preview/wt-respawn", liveSpec, 0)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if second.State == StateFailed {
		t.Fatalf("second start returned stale Failed snapshot — eviction is broken")
	}
	if second.Port == first.Port {
		t.Fatalf("second start reused the same port (%d) — likely no fresh spawn", first.Port)
	}
	waitForStateVia(t, m, wid, dir, StateReady, 5*time.Second)
}

// waitForStateVia polls Manager.Status (not running.snapshot directly)
// so tests stay on the public surface.
func waitForStateVia(t *testing.T, m *Manager, wid, workDir string, want State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, _ := m.Status(context.Background(), wid, workDir)
		if s.State == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s, _ := m.Status(context.Background(), wid, workDir)
	t.Fatalf("state never reached %s within %s (last=%s)", want, timeout, s.State)
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

// TestManagerProxyBeforeStart confirms the proxy handler 404s when
// no server is running. Defense-in-depth — mobile won't normally hit
// this path but a stale URL after a sprite reboot might.
func TestManagerProxyBeforeStart(t *testing.T) {
	t.Parallel()
	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()

	h := m.ProxyHandler("wt-cold", "/worktrees/wt-cold/preview/proxy")
	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/worktrees/wt-cold/preview/proxy/", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestManagerProxyEndToEnd starts a stub HTTP server via Manager.Start
// (the shell script execs a tiny Go binary that listens on the chosen
// port), then drives a request through ProxyHandler and verifies the
// stub received the rewritten path.
//
// This exercises the full chain: spawn → readiness → proxy lookup →
// path rewrite → upstream dispatch. Covers the "two-hop" worry from
// the plan in a self-contained way (one in-process hop instead of
// gateway+host, but the proxy semantics are identical).
func TestManagerProxyEndToEnd(t *testing.T) {
	t.Parallel()

	// Fake-Metro stub: same HTTP server as the other manager tests.
	// /status returns the readiness sentinel so the probe passes;
	// every other GET echoes the path back so we can assert prefix
	// stripping.
	spec := testSpec(fakeMetroScript(""))

	m := New(Options{StopGrace: 2 * time.Second})
	defer m.Shutdown()

	dir := fixtureExpoWorkDir(t)
	wid := "wt-proxy"

	status, err := m.startWithSpec(context.Background(), wid, dir, "http://localhost:8080/preview/test", spec, 0)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if status.Port == 0 {
		t.Fatalf("start returned zero port")
	}

	// Wait for state to flip to Ready (probe hits /status on the stub).
	deadline := time.Now().Add(5 * time.Second)
	var lastState State
	for time.Now().Before(deadline) {
		s, _ := m.Status(context.Background(), wid, dir)
		lastState = s.State
		if lastState == StateReady {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastState != StateReady {
		t.Fatalf("state never reached ready within 5s (last=%s); proxy request below would just surface as 502/refused", lastState)
	}

	// Drive a request through ProxyHandler.
	const prefix = "/worktrees/wt-proxy/preview/proxy"
	h := m.ProxyHandler(wid, prefix)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + prefix + "/hello/world?x=1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "path=/hello/world") {
		t.Errorf("body = %q, want prefix path=/hello/world", body)
	}
}

// TestManagerShutdownStopsAll spawns two stubs and confirms Shutdown
// reaps both process trees (Change 2 again, at the manager level).
func TestManagerShutdownStopsAll(t *testing.T) {
	t.Parallel()
	spec := testSpec(fakeMetroScript("(sleep 30 &)"))

	m := New(Options{StopGrace: 2 * time.Second})

	dir := fixtureExpoWorkDir(t)
	_, err := m.startWithSpec(context.Background(), "wt-1", dir, "http://localhost:8080/preview/wt-1", spec, 0)
	if err != nil {
		t.Fatalf("start wt-1: %v", err)
	}
	_, err = m.startWithSpec(context.Background(), "wt-2", dir, "http://localhost:8080/preview/wt-2", spec, 0)
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
