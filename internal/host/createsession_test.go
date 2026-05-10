package host_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

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


// TestCreateSession_WorktreeRef_Success exercises the new sync-path:
// when a worktree has been migrated to ~/work/<id>/, a session with
// only WorktreeID set resolves to that directory.
func TestCreateSession_WorktreeRef_Success(t *testing.T) {
	// Override the host's workRoot lookup. NOT parallel because the
	// override is a package-level singleton.
	tmpHome := t.TempDir()
	prev := host.SetWorkRootForTest(filepath.Join(tmpHome, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	worktreeID := "01HTEST123"
	dir := filepath.Join(tmpHome, "work", worktreeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// initGitRepo expects to create the dir; instead init in place.
	if out, err := exec.Command("git", "-C", dir, "init", "--initial-branch=main", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	for _, args := range [][]string{
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}

	svc := newTestService(t)
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{WorktreeID: worktreeID},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-wt", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

// TestCreateSession_WorktreeRef_MissingErrors guards the explicit
// "no fall back to clone" contract: WorktreeID for which no
// ~/work/<id>/ has been materialized yields a clear error pointing
// at MigrateWorktree.
func TestCreateSession_WorktreeRef_MissingErrors(t *testing.T) {
	tmpHome := t.TempDir()
	prev := host.SetWorkRootForTest(filepath.Join(tmpHome, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	svc := newTestService(t)
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{WorktreeID: "01HTESTMISSING"},
		Prompt:  "hi",
	}
	_, _, err := svc.CreateSession(context.Background(), "sid-missing", req)
	if err == nil {
		t.Fatal("expected error for unmigrated worktree, got nil")
	}
	if !strings.Contains(err.Error(), "MigrateWorktree") {
		t.Fatalf("expected error to point at MigrateWorktree, got: %v", err)
	}
}
