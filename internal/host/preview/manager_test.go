//go:build unix

package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
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
		Kind:                 KindExpo,
		CmdTemplate:          argv,
		ShouldSubstitutePort: true,
		ReadyProbe:           expoReadyProbe,
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

// TestManagerStartRequiresSetup confirms Start fast-fails on a
// non-Expo worktree without launch configuration.
func TestManagerStartRequiresSetup(t *testing.T) {
	t.Parallel()
	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()

	plain := t.TempDir() // no package.json
	_, err := m.Start(context.Background(), "wt-empty", plain, "")
	if !errors.Is(err, ErrSetupRequired) {
		t.Fatalf("Start on non-Expo dir: got %v, want ErrSetupRequired", err)
	}
}

func TestManagerStartsConfiguredWebPreview(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for the managed-preview integration fixture")
	}
	t.Setenv("CLANK_DIR", t.TempDir())

	dir := t.TempDir()
	writePreviewLaunchConfig(t, dir, `default: web
previews:
  web:
    directory: .
    command: echo "$PORT" >/dev/null; exit 1
    ready:
      path: /
`)

	m := New(Options{StopGrace: time.Second})
	defer m.Shutdown()
	failed, err := m.Start(context.Background(), "wt-configured-web", dir, "web")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	key := serviceKey{WorktreeID: "wt-configured-web", ServiceName: "web"}
	m.mu.Lock()
	r := m.servers[key]
	m.mu.Unlock()
	if r == nil {
		t.Fatal("configured server was not registered by its launch name")
	}
	waitForState(t, r, StateFailed, 5*time.Second)

	writePreviewLaunchConfig(t, dir, `default: web
previews:
  web:
    directory: .
    command: python3 -m http.server "$PORT" --bind 127.0.0.1
    ready:
      path: /
`)
	status, err := m.Start(context.Background(), "wt-configured-web", dir, "web")
	if err != nil {
		t.Fatalf("restart after failed command: %v", err)
	}
	if status.Kind != KindWeb || status.ServiceName != "web" || status.Port == 0 || status.Port == failed.Port {
		t.Fatalf("restart status = %+v; failed status = %+v", status, failed)
	}
	m.mu.Lock()
	r = m.servers[key]
	m.mu.Unlock()
	if r == nil {
		t.Fatal("healthy configured server was not registered")
	}
	waitForState(t, r, StateReady, 5*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", status.Port))
	if err != nil {
		t.Fatalf("GET configured preview: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read response: read=%v close=%v", readErr, closeErr)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Directory listing") {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
	if err := m.StopService("wt-configured-web", "web"); err != nil {
		t.Fatalf("StopService: %v", err)
	}
}

// TestManagerLogTailResolvesConfiguredDefault pins a regression: an
// unnamed configured web preview registers under its launch name, but
// LogTail used to always look up the Expo-only "default" key and
// silently returned nothing for it. Regression for cubic review on #209.
func TestManagerLogTailResolvesConfiguredDefault(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for the managed-preview integration fixture")
	}
	t.Setenv("CLANK_DIR", t.TempDir())

	dir := t.TempDir()
	writePreviewLaunchConfig(t, dir, `default: web
previews:
  web:
    directory: .
    command: python3 -m http.server "$PORT" --bind 127.0.0.1
    ready:
      path: /
`)

	m := New(Options{StopGrace: time.Second})
	defer m.Shutdown()

	wid := "wt-logtail-default"
	status, err := m.Start(context.Background(), wid, dir, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if status.ServiceName != "web" {
		t.Fatalf("status.ServiceName = %q, want %q", status.ServiceName, "web")
	}

	key := serviceKey{WorktreeID: wid, ServiceName: "web"}
	m.mu.Lock()
	r := m.servers[key]
	m.mu.Unlock()
	if r == nil {
		t.Fatal("configured server was not registered under its launch name")
	}
	waitForState(t, r, StateReady, 5*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", status.Port))
	if err != nil {
		t.Fatalf("GET configured preview: %v", err)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Fatalf("close response body: %v", closeErr)
	}

	logs := m.LogTail(wid, dir)
	if len(logs) == 0 {
		t.Fatal("LogTail returned no output for an unnamed configured preview")
	}
	if string(logs) != string(m.LogTailNamed(wid, "web")) {
		t.Fatal("LogTail did not resolve to the configured default service's logs")
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
	if s2.Available || !s2.SetupRequired || s2.State != StateStopped {
		t.Errorf("plain dir status = %+v; want Available=false, SetupRequired=true, State=stopped", s2)
	}
}

// TestManagerReaperSparesTouchedServers pins the liveness contract the
// LAN `clank preview` keepalive depends on: Status reads and idempotent
// re-Starts count as activity. Regression — lastTouch used to be set
// only at spawn, so the reaper killed every LAN preview idleTimeout
// after start even while the CLI and phone were actively using it
// (their Metro traffic never crosses the daemon).
func TestManagerReaperSparesTouchedServers(t *testing.T) {
	t.Parallel()
	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()

	dir := fixtureExpoWorkDir(t)
	spec := testSpec(fakeMetroScript(""))
	wids := []string{"wt-status-polled", "wt-restarted", "wt-untouched"}
	for _, wid := range wids {
		if _, err := m.startWithSpec(context.Background(), wid, dir, "default", spec, 0); err != nil {
			t.Fatalf("start %s: %v", wid, err)
		}
	}

	// Age every record past the idle cutoff, then touch two of them via
	// the paths the CLI keepalive and phone polling actually hit.
	stale := time.Now().Add(-2 * m.idleTimeout)
	m.mu.Lock()
	for _, r := range m.servers {
		r.mu.Lock()
		r.lastTouch = stale
		r.mu.Unlock()
	}
	m.mu.Unlock()

	if _, err := m.Status(context.Background(), "wt-status-polled", dir); err != nil {
		t.Fatalf("status poll: %v", err)
	}
	if _, err := m.startWithSpec(context.Background(), "wt-restarted", dir, "default", spec, 0); err != nil {
		t.Fatalf("idempotent restart: %v", err)
	}

	m.reapIdle()

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, wid := range []string{"wt-status-polled", "wt-restarted"} {
		if _, ok := m.servers[serviceKey{WorktreeID: wid, ServiceName: "default"}]; !ok {
			t.Errorf("%s was reaped despite being touched after going stale", wid)
		}
	}
	if _, ok := m.servers[serviceKey{WorktreeID: "wt-untouched", ServiceName: "default"}]; ok {
		t.Errorf("wt-untouched survived the reaper despite being stale")
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
