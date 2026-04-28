package daemoncli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/host"
	hostclient "github.com/acksell/clank/internal/host/client"
	locallauncher "github.com/acksell/clank/internal/host/launcher/local"
	hostmux "github.com/acksell/clank/internal/host/mux"
	hub "github.com/acksell/clank/internal/hub"
	hubclient "github.com/acksell/clank/internal/hub/client"
	"github.com/acksell/clank/internal/store"
	clanksync "github.com/acksell/clank/internal/sync"
)

// TestRunHubServer_ManagesSocketAndPIDFiles verifies the lifecycle
// concerns lifted out of hub.Service in Phase 2F: runHubServer must
// create hub.sock and hub.pid on startup and remove both on Stop.
//
// This is the new home for the on-disk lifecycle coverage previously
// provided by hub_test's TestDaemonPIDFile (deleted because hub.Service
// no longer touches the filesystem).
func TestRunHubServer_ManagesSocketAndPIDFiles(t *testing.T) {
	// Redirect config.Dir() to a short HOME so we don't touch the real
	// ~/.clank during tests AND keep the resulting hub.sock path under
	// macOS's 104-char Unix socket limit. Cannot t.Parallel: HOME is
	// process-global.
	home, err := os.MkdirTemp("/tmp", "clank-srv-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)

	sockPath, err := hubclient.SocketPath()
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	pidPath, err := hubclient.PIDPath()
	if err != nil {
		t.Fatalf("PIDPath: %v", err)
	}
	if filepath.Base(sockPath) != "hub.sock" {
		t.Errorf("expected socket name hub.sock, got %s", filepath.Base(sockPath))
	}
	if filepath.Base(pidPath) != "hub.pid" {
		t.Errorf("expected pid name hub.pid, got %s", filepath.Base(pidPath))
	}

	// runHubServer expects the parent directory to exist (matches
	// daemoncli.go which MkdirAll's it before calling).
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	s := hub.New()

	// runHubServer requires a host client (Decision #3: no in-process
	// shortcut). Wire a real host.Service behind an httptest server so
	// the lifecycle path under test (socket+PID file management) runs
	// the same control flow as production clankd.
	hostSvc := host.New(host.Options{BackendManagers: s.BackendManagers})
	if err := hostSvc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("host.Run: %v", err)
	}
	t.Cleanup(hostSvc.Shutdown)
	hostSrv := httptest.NewServer(hostmux.New(hostSvc, nil).Handler())
	t.Cleanup(hostSrv.Close)
	s.SetHostClient(hostclient.NewHTTP(hostSrv.URL, nil))

	errCh := make(chan error, 1)
	go func() { errCh <- runHubServer(s, ServerOptions{}) }()

	// Wait for both files to appear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, sErr := os.Stat(sockPath)
		_, pErr := os.Stat(pidPath)
		if sErr == nil && pErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket %s not created: %v", sockPath, err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("pid file %s not created: %v", pidPath, err)
	}

	// Sanity: the live hub answers a simple wire request.
	client := hubclient.NewClient(sockPath)
	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx); err != nil {
		t.Fatalf("Ping over socket: %v", err)
	}

	// Trigger graceful shutdown via the same path SIGINT/SIGTERM uses.
	s.Stop()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runHubServer returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runHubServer did not return after Stop")
	}

	// Both artifacts must be cleaned up.
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket %s should be removed after stop, stat err=%v", sockPath, err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("pid file %s should be removed after stop, stat err=%v", pidPath, err)
	}
}

// TestRunHubServer_TCPMode verifies the TCP listener path: starts on a
// chosen address, refuses requests without a bearer token, accepts
// requests with the right token, and cleans up the PID file on stop.
// No Unix socket is created in this mode.
func TestRunHubServer_TCPMode(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "clank-tcp-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("CLANK_DIR", filepath.Join(home, ".clank"))

	dir, err := config.Dir()
	if err != nil {
		t.Fatalf("config.Dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	// Write a preferences file with the bearer token. The TCP server
	// loads this on startup; without it the server refuses to listen.
	const token = "test-token-abcdef"
	prefs := config.Preferences{
		RemoteHub: &config.RemoteHubPreference{AuthToken: token},
	}
	if err := config.SavePreferences(prefs); err != nil {
		t.Fatalf("save preferences: %v", err)
	}

	// Pick a free port by listening, closing, and reusing the address.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	s := hub.New()
	hostSvc := host.New(host.Options{BackendManagers: s.BackendManagers})
	if err := hostSvc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("host.Init: %v", err)
	}
	t.Cleanup(hostSvc.Shutdown)
	hostSrv := httptest.NewServer(hostmux.New(hostSvc, nil).Handler())
	t.Cleanup(hostSrv.Close)
	s.SetHostClient(hostclient.NewHTTP(hostSrv.URL, nil))

	errCh := make(chan error, 1)
	go func() {
		errCh <- runHubServer(s, ServerOptions{Listen: "tcp://" + addr})
	}()

	// Wait for the listener to be reachable.
	pidPath, err := hubclient.PIDPath()
	if err != nil {
		t.Fatalf("PIDPath: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// PID file is written even in TCP mode (so `clankd stop` works).
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("pid file %s not created: %v", pidPath, err)
	}
	// Unix socket must NOT be created in TCP mode.
	sockPath, _ := hubclient.SocketPath()
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("Unix socket %s should not exist in TCP mode (stat err=%v)", sockPath, err)
	}

	// Unauthenticated request: 401.
	resp, err := http.Get("http://" + addr + "/ping")
	if err != nil {
		t.Fatalf("unauthenticated GET /ping: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated request: want 401, got %d", resp.StatusCode)
	}

	// Wrong token: 401.
	req, _ := http.NewRequest("GET", "http://"+addr+"/ping", nil)
	req.Header.Set("Authorization", "Bearer not-the-right-one")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("wrong-token GET /ping: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: want 401, got %d", resp.StatusCode)
	}

	// Right token: 200 + JSON body.
	req, _ = http.NewRequest("GET", "http://"+addr+"/ping", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticated GET /ping: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("authenticated request: want 200, got %d", resp.StatusCode)
	}
	var pong map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&pong); err != nil {
		t.Errorf("decode /ping response: %v", err)
	}
	resp.Body.Close()
	if pong["status"] == nil {
		t.Errorf("/ping response missing status field: %#v", pong)
	}

	s.Stop()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runHubServer returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runHubServer did not return after Stop")
	}

	// PID file cleaned up.
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("pid file %s should be removed after stop, stat err=%v", pidPath, err)
	}
}

// TestRunHubServer_TCPMode_ReceivesBundle is the Step 2 end-to-end check:
// a TCP-mode hub accepts a bundle over POST /sync/repos/{key}/bundle,
// unbundles it into the on-disk mirror, persists synced_repos /
// synced_branches rows, and lists them via GET /sync/repos.
func TestRunHubServer_TCPMode_ReceivesBundle(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "clank-tcp-bundle-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("CLANK_DIR", filepath.Join(home, ".clank"))

	dir, err := config.Dir()
	if err != nil {
		t.Fatalf("config.Dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	const token = "bundle-test-token"
	prefs := config.Preferences{
		RemoteHub: &config.RemoteHubPreference{AuthToken: token},
	}
	if err := config.SavePreferences(prefs); err != nil {
		t.Fatalf("save preferences: %v", err)
	}

	// Real Store (the receiver persists into it).
	st, err := store.Open(filepath.Join(dir, "clank.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	s := hub.New()
	s.Store = st
	hostSvc := host.New(host.Options{BackendManagers: s.BackendManagers})
	if err := hostSvc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("host.Init: %v", err)
	}
	t.Cleanup(hostSvc.Shutdown)
	hostSrv := httptest.NewServer(hostmux.New(hostSvc, nil).Handler())
	t.Cleanup(hostSrv.Close)
	s.SetHostClient(hostclient.NewHTTP(hostSrv.URL, nil))

	errCh := make(chan error, 1)
	go func() {
		errCh <- runHubServer(s, ServerOptions{Listen: "tcp://" + addr})
	}()

	// Wait for listener.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Build a bundle from a fresh source repo.
	src := t.TempDir()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	runGit := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return out
	}
	runGit("init", "-b", "feat/x", src)
	if err := os.WriteFile(filepath.Join(src, "marker.txt"), []byte("synced!\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit("-C", src, "add", "marker.txt")
	runGit("-C", src, "commit", "-m", "initial")
	tipOut := runGit("-C", src, "rev-parse", "feat/x")
	tip := strings.TrimSpace(string(tipOut))

	bundlePath := filepath.Join(t.TempDir(), "feat-x.bundle")
	runGit("-C", src, "bundle", "create", bundlePath, "feat/x")
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	const remoteURL = "https://github.com/example/proj.git"
	repoKey := clanksync.RepoKey(remoteURL)

	// POST without auth: 401.
	req, _ := http.NewRequest("POST", "http://"+addr+"/sync/repos/"+repoKey+"/bundle", bytes.NewReader(bundleBytes))
	req.Header.Set("X-Clank-Branch", "feat/x")
	req.Header.Set("X-Clank-Remote-URL", remoteURL)
	req.Header.Set("X-Clank-Tip-SHA", tip)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated bundle: want 401, got %d", resp.StatusCode)
	}

	// POST with auth: 204.
	req, _ = http.NewRequest("POST", "http://"+addr+"/sync/repos/"+repoKey+"/bundle", bytes.NewReader(bundleBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-git-bundle")
	req.Header.Set("X-Clank-Branch", "feat/x")
	req.Header.Set("X-Clank-Remote-URL", remoteURL)
	req.Header.Set("X-Clank-Tip-SHA", tip)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticated POST: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("authenticated bundle: want 204, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Listing reflects the new repo and branch.
	req, _ = http.NewRequest("GET", "http://"+addr+"/sync/repos", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sync/repos: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list: want 200, got %d", resp.StatusCode)
	}
	var repos []clanksync.SyncedRepoView
	if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(repos) != 1 || repos[0].RepoKey != repoKey {
		t.Errorf("list payload wrong: %+v", repos)
	}
	if len(repos[0].Branches) != 1 || repos[0].Branches[0].Branch != "feat/x" || repos[0].Branches[0].TipSHA != tip {
		t.Errorf("list branches wrong: %+v", repos[0].Branches)
	}

	// The mirror on disk must be a valid git repo containing our marker.
	mirrorPath := filepath.Join(dir, "sync", repoKey, "repo.git")
	cloneDest := t.TempDir()
	clone := exec.Command("git", "clone", "-b", "feat/x", mirrorPath, filepath.Join(cloneDest, "out"))
	clone.Env = gitEnv
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("git clone from mirror: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(cloneDest, "out", "marker.txt"))
	if err != nil {
		t.Fatalf("read clone marker: %v", err)
	}
	if string(got) != "synced!\n" {
		t.Errorf("marker contents wrong: %q", got)
	}

	// And the smart-HTTP endpoint must allow `git clone` over the wire
	// with the bearer token attached as an extra header — this is what
	// a Daytona sandbox will do (Step 6).
	httpClone := filepath.Join(cloneDest, "via-http")
	httpURL := "http://" + addr + "/sync/repos/" + repoKey + "/git"
	cmd := exec.Command("git",
		"-c", "http.extraHeader=Authorization: Bearer "+token,
		"clone", "-b", "feat/x", httpURL, httpClone,
	)
	cmd.Env = gitEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone over smart-HTTP: %v\n%s", err, out)
	}
	got, err = os.ReadFile(filepath.Join(httpClone, "marker.txt"))
	if err != nil {
		t.Fatalf("read smart-HTTP clone marker: %v", err)
	}
	if string(got) != "synced!\n" {
		t.Errorf("smart-HTTP clone contents wrong: %q", got)
	}

	s.Stop()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runHubServer returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runHubServer did not return after Stop")
	}
}

// buildClankHostFromTest compiles cmd/clank-host into the test's
// scratch dir. Replicated here (rather than imported from the local
// launcher's test) so tests in this package don't take a build-time
// dep on internal/host/launcher/local/_test.
func buildClankHostFromTest(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "clank-host")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/clank-host")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build clank-host: %v\n%s", err, out)
	}
	return bin
}

// newLocalLauncher wires a launcher whose spawned clank-host pulls
// from a sync source URL with the test's bearer token.
func newLocalLauncher(bin, syncSource, token string) *locallauncher.Launcher {
	return locallauncher.New(locallauncher.Options{
		BinPath:       bin,
		GitSyncSource: syncSource,
		GitSyncToken:  token,
	}, nil)
}

// buildBundleForLaunchTest produces a bundle of a one-commit branch
// "feat/launch" containing a marker file. Returns the bundle bytes and
// the SHA of the branch tip.
func buildBundleForLaunchTest(t *testing.T) ([]byte, string) {
	t.Helper()
	src := t.TempDir()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "feat/launch", src)
	if err := os.WriteFile(filepath.Join(src, "marker.txt"), []byte("launch flow ok\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit("-C", src, "add", "marker.txt")
	runGit("-C", src, "commit", "-m", "initial")
	tipBytes, err := exec.Command("git", "-C", src, "rev-parse", "feat/launch").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	tip := strings.TrimSpace(string(tipBytes))
	bundlePath := filepath.Join(t.TempDir(), "feat-launch.bundle")
	runGit("-C", src, "bundle", "create", bundlePath, "feat/launch")
	body, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	return body, tip
}


// TestRunHubServer_TCPMode_LaunchHostFlow is the closing E2E smoke for
// the MVP: it pushes a bundle to a cloud-hub-mode hub, then triggers a
// session creation that asks the local-stub launcher to spawn a fresh
// clank-host. The spawned clank-host uses --git-sync-source pointed
// at this same hub to clone the repo from the bundle (no GitHub
// access required). We assert the clone lands on disk with the
// expected marker file.
//
// Backend creation will fail inside the spawned clank-host (no real
// agent backend installed in test), so the POST /sessions response is
// an error. That's fine — the clone happens before the backend step
// and is what proves the sync pipeline is wired end-to-end. The
// assertion is on disk, not on the HTTP response.
func TestRunHubServer_TCPMode_LaunchHostFlow(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "clank-launch-flow-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	// Set both so the spawned clank-host child inherits them and writes
	// its clones into the test's scratch dir, not the user's real home.
	t.Setenv("HOME", home)
	t.Setenv("CLANK_DIR", filepath.Join(home, ".clank"))

	dir, err := config.Dir()
	if err != nil {
		t.Fatalf("config.Dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	const token = "launch-flow-token"
	if err := config.SavePreferences(config.Preferences{
		RemoteHub: &config.RemoteHubPreference{AuthToken: token},
	}); err != nil {
		t.Fatalf("save preferences: %v", err)
	}

	st, err := store.Open(filepath.Join(dir, "clank.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()
	publicURL := "http://" + addr

	s := hub.New()
	s.Store = st

	// Cloud hub also acts as the parent for spawned clank-host children.
	// Wire the launcher with the same auth + URL combo a real
	// production deployment would use: spawned clank-host clones from
	// publicURL with `Authorization: Bearer <token>`.
	bin := buildClankHostFromTest(t)
	launcher := newLocalLauncher(bin, publicURL, token)
	s.SetHostLauncher("local-stub", launcher)
	t.Cleanup(launcher.Stop)

	// Minimal local host; not exercised in this test path because the
	// session is dispatched to the launched host instead.
	hostSvc := host.New(host.Options{BackendManagers: s.BackendManagers})
	if err := hostSvc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("host.Init: %v", err)
	}
	t.Cleanup(hostSvc.Shutdown)
	hostSrv := httptest.NewServer(hostmux.New(hostSvc, nil).Handler())
	t.Cleanup(hostSrv.Close)
	s.SetHostClient(hostclient.NewHTTP(hostSrv.URL, nil))

	errCh := make(chan error, 1)
	go func() { errCh <- runHubServer(s, ServerOptions{Listen: "tcp://" + addr}) }()

	// Wait for cloud hub to accept connections.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Build and push a bundle for an entirely fictitious origin URL —
	// the laptop's real origin is irrelevant; what matters is that the
	// cloud hub mirror has the data when the spawned host asks for it.
	const fakeOrigin = "https://example.invalid/clank/launch-flow.git"
	repoKey := clanksync.RepoKey(fakeOrigin)
	bundleBytes, tip := buildBundleForLaunchTest(t)

	req, _ := http.NewRequest("POST",
		"http://"+addr+"/sync/repos/"+repoKey+"/bundle",
		bytes.NewReader(bundleBytes),
	)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-git-bundle")
	req.Header.Set("X-Clank-Branch", "feat/launch")
	req.Header.Set("X-Clank-Remote-URL", fakeOrigin)
	req.Header.Set("X-Clank-Tip-SHA", tip)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("push bundle: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("bundle push: want 204, got %d", resp.StatusCode)
	}

	// Trigger session-create with launch_host: local-stub. Backend
	// creation will fail in the spawned host (no opencode/claude-code
	// binary available in test), but workDirFor runs first, so the
	// clone lands on disk before the failure.
	body := strings.NewReader(`{
		"git_ref": {"remote_url": "` + fakeOrigin + `", "worktree_branch": "feat/launch"},
		"prompt": "noop",
		"backend": "claude-code",
		"launch_host": {"provider": "local-stub"}
	}`)
	req, _ = http.NewRequest("POST", "http://"+addr+"/sessions", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("/sessions response: %d %s", resp.StatusCode, respBody)

	// Whatever the backend-creation outcome, the spawned host should
	// have completed its clone. The clone lands in the spawned host's
	// clones dir; since the child inherits HOME from us, that's
	// $HOME/.clank/clones/<sanitized-url>.
	cloneName, err := agent.CloneDirName(fakeOrigin)
	if err != nil {
		t.Fatalf("CloneDirName: %v", err)
	}
	clonesDir := filepath.Join(home, ".clank", "clones")

	// Give the spawned host a brief window to finish the clone.
	clonePath := filepath.Join(clonesDir, cloneName)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(clonePath, "marker.txt")); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, err := os.ReadFile(filepath.Join(clonePath, "marker.txt"))
	if err != nil {
		t.Fatalf("clone marker: %v", err)
	}
	if string(got) != "launch flow ok\n" {
		t.Errorf("marker contents wrong: %q", got)
	}

	s.Stop()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runHubServer error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runHubServer did not return after Stop")
	}
}

// TestRunHubServer_TCPMode_RequiresAuthToken locks in the safety check:
// listening on TCP without a configured bearer token must fail fast.
// A TCP listener with no auth would expose every Clank session to anyone
// on the network.
func TestRunHubServer_TCPMode_RequiresAuthToken(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "clank-tcp-noauth-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("CLANK_DIR", filepath.Join(home, ".clank"))

	dir, err := config.Dir()
	if err != nil {
		t.Fatalf("config.Dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	// Intentionally no preferences.json — the safety check must fire.

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	s := hub.New()
	hostSvc := host.New(host.Options{BackendManagers: s.BackendManagers})
	if err := hostSvc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("host.Init: %v", err)
	}
	t.Cleanup(hostSvc.Shutdown)
	hostSrv := httptest.NewServer(hostmux.New(hostSvc, nil).Handler())
	t.Cleanup(hostSrv.Close)
	s.SetHostClient(hostclient.NewHTTP(hostSrv.URL, nil))

	err = runHubServer(s, ServerOptions{Listen: "tcp://" + addr})
	if err == nil {
		t.Fatal("expected error from runHubServer with no auth token, got nil")
	}
	if !strings.Contains(err.Error(), "auth_token") {
		t.Errorf("expected error mentioning auth_token, got: %v", err)
	}
}
