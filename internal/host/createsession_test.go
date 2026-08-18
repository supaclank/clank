package host_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/git"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/host/store"
)

// TestCreateSession_LocalRef_Success exercises the §7 happy path: a
// GitRef.Local pointing at an existing git repo root resolves to a
// workdir = that path verbatim. There is no host repo registry to
// consult.
func TestCreateSession_LocalRef_Success(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	dir := initGitRepo(t, "git@github.com:supaclank/clank.git")
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-local", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func TestCreateSession_HarnessMustBeAllowed(t *testing.T) {
	t.Parallel()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		BackendAllowed: func(agent.BackendType) bool { return false },
	})
	t.Cleanup(svc.Shutdown)

	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: "/permission-check-happens-first"},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-disallowed", req); !errors.Is(err, host.ErrBackendNotAllowed) {
		t.Fatalf("CreateSession error = %v, want ErrBackendNotAllowed", err)
	}
}

func TestLiveSession_HarnessPermissionIsRechecked(t *testing.T) {
	t.Parallel()
	allowed := true
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		BackendAllowed: func(agent.BackendType) bool { return allowed },
	})
	t.Cleanup(svc.Shutdown)

	dir := initGitRepo(t, "git@github.com:supaclank/clank.git")
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-revoked", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	allowed = false
	if err := svc.SendMessage(context.Background(), "sid-revoked", agent.SendMessageOpts{Text: "hello"}); !errors.Is(err, host.ErrBackendNotAllowed) {
		t.Fatalf("SendMessage error = %v, want ErrBackendNotAllowed", err)
	}
}

// TestSessionMessages_HarnessPermissionIsRechecked pins the cached-backend
// read path: revoking a harness must stop history reads even while the
// backend stays warm in the registry, not just on the next rehydrate.
func TestSessionMessages_HarnessPermissionIsRechecked(t *testing.T) {
	t.Parallel()
	allowed := true
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		BackendAllowed: func(agent.BackendType) bool { return allowed },
	})
	t.Cleanup(svc.Shutdown)

	dir := initGitRepo(t, "git@github.com:supaclank/clank.git")
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-revoked-messages", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	allowed = false
	if _, err := svc.SessionMessages(context.Background(), "sid-revoked-messages"); !errors.Is(err, host.ErrBackendNotAllowed) {
		t.Fatalf("SessionMessages error = %v, want ErrBackendNotAllowed", err)
	}
}

// TestPendingPermissions_HarnessPermissionIsRechecked mirrors
// TestSessionMessages_HarnessPermissionIsRechecked for the pending-permission
// read path.
func TestPendingPermissions_HarnessPermissionIsRechecked(t *testing.T) {
	t.Parallel()
	allowed := true
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		BackendAllowed: func(agent.BackendType) bool { return allowed },
	})
	t.Cleanup(svc.Shutdown)

	dir := initGitRepo(t, "git@github.com:supaclank/clank.git")
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-revoked-pending", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	allowed = false
	if _, err := svc.PendingPermissions(context.Background(), "sid-revoked-pending"); !errors.Is(err, host.ErrBackendNotAllowed) {
		t.Fatalf("PendingPermissions error = %v, want ErrBackendNotAllowed", err)
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

// TestCreateSession_LocalRef_AcceptsSubdir pins monorepo support: a
// LocalPath pointing inside a repo (e.g. supaclank/web-app) runs the
// session AT that folder while identity normalizes to the repo root —
// {LocalPath: root, Subdir: "web-app"} — so keying, sidebar grouping,
// and git ops stay repo-level.
func TestCreateSession_LocalRef_AcceptsSubdir(t *testing.T) {
	t.Parallel()
	mgr := &noopBackendManager{}
	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{agent.BackendOpenCode: mgr},
		SessionsStore:   st,
	})
	t.Cleanup(svc.Shutdown)

	root := initGitRepo(t, "git@github.com:supaclank/clank.git")
	sub := filepath.Join(root, "web-app")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: sub},
		Prompt:  "hi",
	}
	_, info, err := svc.CreateSession(context.Background(), "sid-subdir", req)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if mgr.createdWorkDir != sub {
		t.Errorf("backend workdir = %q, want the requested subdir %q", mgr.createdWorkDir, sub)
	}
	want := agent.GitRef{LocalPath: root, Subdir: "web-app", DisplayName: "web-app"}
	if info.GitRef != want {
		t.Errorf("normalized ref = %+v, want %+v", info.GitRef, want)
	}
	persisted, err := st.GetSession(context.Background(), "sid-subdir")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if persisted.GitRef != want {
		t.Errorf("persisted ref = %+v, want %+v", persisted.GitRef, want)
	}
}

// TestCreateSession_LocalRef_SubdirWithBranch pins the two-axis case:
// WorktreeBranch resolves against the repo ROOT (the branch worktree is
// named after the repo, never the subdir) and the subdir then re-applies
// inside that worktree — the session works on branch X of the whole
// repo, from its web-app folder.
func TestCreateSession_LocalRef_SubdirWithBranch(t *testing.T) {
	t.Parallel()
	mgr := &noopBackendManager{}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{agent.BackendOpenCode: mgr},
	})
	t.Cleanup(svc.Shutdown)

	root := initGitRepo(t, "git@github.com:supaclank/clank.git")
	sub := filepath.Join(root, "web-app")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Commit the subdir so the branch worktree materializes it.
	if err := os.WriteFile(filepath.Join(sub, "index.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "web-app"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}

	wtDir, err := git.WorktreeDir(filepath.Base(root), "feat")
	if err != nil {
		t.Fatalf("WorktreeDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(wtDir)) })

	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: sub, WorktreeBranch: "feat"},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-sub-branch", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if want := filepath.Join(wtDir, "web-app"); mgr.createdWorkDir != want {
		t.Errorf("backend workdir = %q, want subdir inside the branch worktree %q", mgr.createdWorkDir, want)
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

// TestCreateSession_WorktreeRef_Subdir pins the remote-sandbox arm of
// GitRef.Subdir: a {worktree_id, subdir} ref runs the session at
// ~/work/<id>/<subdir>, and a subdir that doesn't exist in the worktree
// fails fast with a clear error instead of spawning in a bogus cwd.
func TestCreateSession_WorktreeRef_Subdir(t *testing.T) {
	// NOT parallel — SetWorkRootForTest mutates a package-level global.
	tmpHome := t.TempDir()
	prev := host.SetWorkRootForTest(filepath.Join(tmpHome, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	worktreeID := "01HTESTSUBDIR"
	sub := filepath.Join(tmpHome, "work", worktreeID, "web-app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	mgr := &noopBackendManager{}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{agent.BackendOpenCode: mgr},
	})
	t.Cleanup(svc.Shutdown)

	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{WorktreeID: worktreeID, Subdir: "web-app"},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-wt-sub", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if mgr.createdWorkDir != sub {
		t.Errorf("backend workdir = %q, want %q", mgr.createdWorkDir, sub)
	}

	req.GitRef.Subdir = "does-not-exist"
	_, _, err := svc.CreateSession(context.Background(), "sid-wt-sub-missing", req)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected missing-subdir error, got %v", err)
	}
}

// TestCreateSession_WorktreeRef_MissingErrors guards the explicit
// "no fall back to clone" contract: a WorktreeID for which no
// ~/work/<id>/ exists yields a clear "not present on this host" error.
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
		t.Fatal("expected error for a missing worktree, got nil")
	}
	if !strings.Contains(err.Error(), "not present") {
		t.Fatalf("expected error to say the worktree isn't present on this host, got: %v", err)
	}
}
