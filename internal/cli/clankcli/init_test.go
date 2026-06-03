package clankcli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// TestInitRepoWithClient_TracksWholeRepoIncludingNewWorktrees is the
// end-to-end of repo-wide onboarding: `clank init` marks the repo and
// eagerly registers the active worktree, and a worktree added AFTER init
// auto-registers on its first non-interactive push — no AutoPushAllRepos.
func TestInitRepoWithClient_TracksWholeRepoIncludingNewWorktrees(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir()) // no AutoPushAllRepos
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))

	cli, st := newSyncServer(t)
	dc := daemonclient.NewTCPClient(cli.BaseURL(), "")
	repo := newGitRepo(t)
	cmd, _ := newPromptCmd("") // non-interactive

	ctx := context.Background()
	if err := initRepoWithClient(ctx, cmd, cli, dc, repo); err != nil {
		t.Fatalf("initRepoWithClient: %v", err)
	}

	// Repo is marked, and the active worktree was eagerly registered.
	tracked, err := agent.IsRepoAutoTracked(repo)
	if err != nil || !tracked {
		t.Fatalf("repo not auto-tracked after init (tracked=%v err=%v)", tracked, err)
	}
	mainID, _ := agent.ReadLocalWorktreeID(repo)
	if mainID == "" {
		t.Fatal("active worktree not registered by init")
	}

	// A worktree added AFTER init is not pre-registered...
	pgit(t, repo, "branch", "feature")
	linkedDir := filepath.Join(t.TempDir(), "linked")
	pgit(t, repo, "worktree", "add", linkedDir, "feature")
	if id, _ := agent.ReadLocalWorktreeID(linkedDir); id != "" {
		t.Fatalf("new worktree unexpectedly pre-registered: %q", id)
	}

	// ...but auto-registers on its first push, via the repo marker.
	id, err := ensureTracked(ctx, cmd, cli, linkedDir, "", false)
	if err != nil {
		t.Fatalf("ensureTracked(new worktree): %v", err)
	}
	if id == "" || id == mainID {
		t.Fatalf("new worktree got bad id %q (main=%q)", id, mainID)
	}
	if _, err := st.GetWorktreeByID(ctx, id); err != nil {
		t.Fatalf("new worktree not registered on remote: %v", err)
	}
}

// TestInitRepoWithClient_PushesRecentWorktrees pins that init now performs an
// initial push of each recently-active worktree it onboards — registration
// alone left them with no checkpoint, invisible in clients (the Inbox only
// shows worktrees that have synced sessions) until the next idle push.
func TestInitRepoWithClient_PushesRecentWorktrees(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))

	cli, st := newSyncServer(t)
	dc := daemonclient.NewTCPClient(cli.BaseURL(), "")
	repo := newGitRepo(t)
	cmd, _ := newPromptCmd("")

	ctx := context.Background()
	if err := initRepoWithClient(ctx, cmd, cli, dc, repo); err != nil {
		t.Fatalf("initRepoWithClient: %v", err)
	}

	mainID, _ := agent.ReadLocalWorktreeID(repo)
	if mainID == "" {
		t.Fatal("active worktree not registered by init")
	}
	wt, err := st.GetWorktreeByID(ctx, mainID)
	if err != nil {
		t.Fatalf("get worktree: %v", err)
	}
	if wt.LatestSyncedCheckpoint == "" {
		t.Fatal("init did not push a checkpoint for the active worktree (registration without push)")
	}
}
