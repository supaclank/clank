package daemoncli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	hub "github.com/acksell/clank/internal/hub"
	hubclient "github.com/acksell/clank/internal/hub/client"
)

// TestE2E_HubHostOpenCode_StartsSession is the only test in the repo that
// drives the full production wire end-to-end:
//
//	hubclient → hub.sock → hub.Service → hostclient.HTTP → host.sock
//	  → clank-host (subprocess) → host.Service → OpenCodeBackendManager
//	  → real `opencode serve` subprocess
//
// Every other integration test stubs at least one boundary (httptest TCP
// instead of unix sockets, in-process host instead of clank-host, stub
// BackendManager instead of opencode). This test exists to catch wiring
// bugs that only manifest when every boundary is real — exactly the
// class of bug that motivated splitting the daemon into Hub + Host
// processes (see hub_host_refactor_code_review.md §7).
//
// The test does NOT assert anything about opencode's behavior beyond
// "the wire reached it and a session was created". opencode may fail
// to do useful work without API keys; that's not what this test covers.
//
// Skipped when:
//   - go test -short (the go-build of clank-host alone takes a few seconds)
//   - opencode is not on PATH
//
// Cannot run with t.Parallel: HOME is process-global and the test
// installs a temp HOME so config.Dir() resolves to scratch space (also
// keeps the hub.sock path under macOS's 104-char unix-socket limit).
func TestE2E_HubHostOpenCode_StartsSession(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test: requires building clank-host")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on PATH; skipping e2e test")
	}

	// HOME under /tmp keeps hub.sock path short on macOS (104-char limit).
	home, err := os.MkdirTemp("/tmp", "clank-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)

	// Build clank-host into a temp dir and override binary resolution.
	// Same pattern as TestStartHost_SpawnsAndServes — guarantees the
	// binary matches the source under test.
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "clank-host")
	build := exec.Command("go", "build", "-o", binPath, "../../../cmd/clank-host")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build clank-host: %v", err)
	}
	t.Setenv("CLANK_HOST_BIN", binPath)

	// Match daemoncli.RunStart(true): create config dir, spawn host,
	// wire host client, start hub server.
	configDir := filepath.Join(home, ".clank")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hh, err := startHost(ctx, configDir, io.Discard)
	if err != nil {
		t.Fatalf("startHost: %v", err)
	}
	t.Cleanup(hh.stop)

	d := hub.New()
	d.SetHostClient(hh.client)

	hubErr := make(chan error, 1)
	go func() { hubErr <- runHubServer(d, ServerOptions{}) }()
	t.Cleanup(func() {
		d.Stop()
		select {
		case <-hubErr:
		case <-time.After(5 * time.Second):
			t.Errorf("runHubServer did not return after Stop")
		}
	})

	// Wait for hub.sock to be reachable.
	hc, err := hubclient.NewDefaultClient()
	if err != nil {
		t.Fatalf("NewDefaultClient: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := hc.Ping(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub.sock not reachable within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Create a real local git repo for the session to attach to. No
	// remote needed — the local-add path (§7.5 step 3) requires only
	// that GitRef.Path points to a valid git repository.
	repo := initGitRepoLocal(t)

	// Drive the full stack: hubclient → hub.sock → ... → opencode.
	createCtx, ccancel := context.WithTimeout(ctx, 20*time.Second)
	defer ccancel()
	info, err := hc.Sessions().Create(createCtx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: repo},
		Prompt:  "noop",
	})
	if err != nil {
		t.Fatalf("Sessions().Create over full e2e wire: %v", err)
	}
	if info.ID == "" {
		t.Fatal("created session has empty ID")
	}
	if info.GitRef.LocalPath == "" || info.GitRef.LocalPath != repo {
		t.Errorf("GitRef = %+v, want Local.Path=%q", info.GitRef, repo)
	}

	// Round-trip: the session must show up in List, proving the hub
	// actually persisted the managed session post-create.
	listed, err := hc.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("Sessions().List: %v", err)
	}
	var found *agent.SessionInfo
	for i := range listed {
		if listed[i].ID == info.ID {
			found = &listed[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("created session %s not in List response (got %d sessions)", info.ID, len(listed))
	}
	if found.Backend != agent.BackendOpenCode {
		t.Errorf("listed session Backend = %q, want %q", found.Backend, agent.BackendOpenCode)
	}
}

// initGitRepoLocal creates a minimal git repo with one commit. Unlike
// initGitRepo in the host_test package this does NOT add an "origin"
// remote — local-kind GitRefs are keyed off the path, not the remote.
func initGitRepoLocal(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "initial")
	return dir
}
