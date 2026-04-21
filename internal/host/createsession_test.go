package host_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// newTestServiceWithClonesDir builds a Service with a temp clones_dir so
// remote-ref tests don't pollute ~/.clank/clones.
func newTestServiceWithClonesDir(t *testing.T) (*host.Service, string) {
	t.Helper()
	clonesDir := t.TempDir()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		ClonesDir: clonesDir,
	})
	t.Cleanup(svc.Shutdown)
	return svc, clonesDir
}

// TestCreateSession_LocalRef_Success exercises the §7 happy path: a
// GitRef.Local pointing at an existing git repo root resolves to a
// workdir = that path verbatim. There is no host repo registry to
// consult.
func TestCreateSession_LocalRef_Success(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	dir := initGitRepo(t, "git@github.com:acksell/clank.git")
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-local", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

// TestCreateSession_LocalRef_RejectsNonGit verifies that a Local path
// that isn't a git repo fails fast instead of silently registering bogus
// state.
func TestCreateSession_LocalRef_RejectsNonGit(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	dir := t.TempDir() // not a git repo
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
	}
	_, _, err := svc.CreateSession(context.Background(), "sid-bad", req)
	if err == nil {
		t.Fatal("expected error when local path is not a git repo, got nil")
	}
	if !strings.Contains(err.Error(), "not a git") && !strings.Contains(err.Error(), "repo root") {
		t.Errorf("error = %v, want git/repo-root error", err)
	}
}

// TestCreateSession_LocalRef_RejectsRelativePath ensures the host never
// resolves a relative path against an implicit cwd.
func TestCreateSession_LocalRef_RejectsRelativePath(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: "relative/path"},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-rel", req); err == nil {
		t.Fatal("expected error for relative path, got nil")
	}
}

// TestCreateSession_LocalRef_RejectsSubdir requires the Local path to be
// the repo root, not a subdirectory inside it. Accepting a subdirectory
// would silently change the worktree base.
func TestCreateSession_LocalRef_RejectsSubdir(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	root := initGitRepo(t, "git@github.com:acksell/clank.git")
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: sub},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-sub", req); err == nil {
		t.Fatal("expected error for non-root dir")
	}
}

// TestCreateSession_BothRefs_PrefersLocalPath is the regression test for
// Bug 3: clients (TUI, clankcli) running on the same host as the daemon
// send BOTH LocalPath and RemoteURL. Before the fix, the host ignored
// LocalPath and always cloned RemoteURL into ~/.clank/clones/, so the
// agent ran against a fresh clone instead of the user's working tree.
//
// The contract: when LocalPath is set, exists, and is a repo root, the
// host MUST use it directly and MUST NOT touch ClonesDir.
func TestCreateSession_BothRefs_PrefersLocalPath(t *testing.T) {
	t.Parallel()
	svc, clonesDir := newTestServiceWithClonesDir(t)

	const remoteURL = "git@github.com:acksell/clank.git"
	dir := initGitRepo(t, remoteURL)

	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: dir, RemoteURL: remoteURL},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-bug3", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	entries, err := os.ReadDir(clonesDir)
	if err != nil {
		t.Fatalf("read clonesDir: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected no clones (LocalPath should win), got %v", names)
	}
}

// TestCreateSession_BothRefs_FallsBackToCloneWhenLocalMissing covers the
// remote-host scenario: a TUI on machine A targeting a daemon on machine
// B sends both fields, but A's LocalPath does not exist on B. The host
// must silently fall through to cloning RemoteURL.
func TestCreateSession_BothRefs_FallsBackToCloneWhenLocalMissing(t *testing.T) {
	t.Parallel()
	svc, clonesDir := newTestServiceWithClonesDir(t)

	source := initGitRepo(t, "git@github.com:acksell/clank.git")
	sourceURL := "file://" + source

	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef: agent.GitRef{
			LocalPath: "/nonexistent/path/on/this/host",
			RemoteURL: sourceURL,
		},
		Prompt: "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-fallback", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	entries, err := os.ReadDir(clonesDir)
	if err != nil {
		t.Fatalf("read clonesDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 clone (LocalPath missing → fall back), got %d", len(entries))
	}
}

// TestCreateSession_RemoteRef_ClonesIntoClonesDir exercises the §7
// auto-clone path: the host derives a deterministic directory from the
// remote URL and clones into it on first use. Uses file:// so no daemon
// is needed.
func TestCreateSession_RemoteRef_ClonesIntoClonesDir(t *testing.T) {
	t.Parallel()
	svc, clonesDir := newTestServiceWithClonesDir(t)

	source := initGitRepo(t, "git@github.com:acksell/clank.git")
	sourceURL := "file://" + source

	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: sourceURL},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-clone", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Verify the host materialized a clone under clonesDir/<deterministic name>/.
	entries, err := os.ReadDir(clonesDir)
	if err != nil {
		t.Fatalf("read clonesDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 clone, got %d", len(entries))
	}
	clonePath := filepath.Join(clonesDir, entries[0].Name())
	if _, err := os.Stat(filepath.Join(clonePath, ".git")); err != nil {
		t.Errorf("clone destination missing .git: %v", err)
	}
}

// TestCreateSession_RemoteRef_ReusesExistingClone verifies the second
// call with the same remote does not re-clone — the resolver finds the
// existing directory and uses it.
func TestCreateSession_RemoteRef_ReusesExistingClone(t *testing.T) {
	t.Parallel()
	svc, clonesDir := newTestServiceWithClonesDir(t)

	source := initGitRepo(t, "git@github.com:acksell/clank.git")
	sourceURL := "file://" + source

	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: sourceURL},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-clone-1", req); err != nil {
		t.Fatalf("CreateSession #1: %v", err)
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-clone-2", req); err != nil {
		t.Fatalf("CreateSession #2: %v", err)
	}

	entries, err := os.ReadDir(clonesDir)
	if err != nil {
		t.Fatalf("read clonesDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected reuse (1 dir), got %d", len(entries))
	}
}
