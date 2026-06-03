package clankcli

import (
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// TestDisableRepo_RemovesMarkerAndUntracksWorktrees: `clank disable --repo`
// undoes `clank init` — it clears the repo marker and the cached id of
// every worktree, so the repo stops auto-tracking.
func TestDisableRepo_RemovesMarkerAndUntracksWorktrees(t *testing.T) {
	t.Setenv(agent.EnvWorktreeID, "") // assert on the on-disk id, not the env override
	repo := newGitRepo(t)
	if err := agent.EnableRepoAutoTrack(repo); err != nil {
		t.Fatal(err)
	}
	if err := agent.WriteLocalWorktreeID(repo, "wt-main"); err != nil {
		t.Fatal(err)
	}
	cmd, out := newPromptCmd("")

	if err := runDisableRepo(cmd, repo); err != nil {
		t.Fatalf("runDisableRepo: %v", err)
	}

	if tracked, _ := agent.IsRepoAutoTracked(repo); tracked {
		t.Error("repo still auto-tracked after `disable --repo`")
	}
	if id, _ := agent.ReadLocalWorktreeID(repo); id != "" {
		t.Errorf("worktree id %q left after `disable --repo`", id)
	}
	if !strings.Contains(out.String(), "Stopped auto-tracking") {
		t.Errorf("unexpected output: %q", out.String())
	}
}

// TestDisableWorktree_WarnsWhenRepoAutoTracked: plain `clank disable` in an
// auto-tracked repo clears this worktree's id but warns that the repo will
// re-register it on the next push — pointing at `disable --repo`.
func TestDisableWorktree_WarnsWhenRepoAutoTracked(t *testing.T) {
	t.Setenv(agent.EnvWorktreeID, "")
	repo := newGitRepo(t)
	if err := agent.EnableRepoAutoTrack(repo); err != nil {
		t.Fatal(err)
	}
	if err := agent.WriteLocalWorktreeID(repo, "wt-main"); err != nil {
		t.Fatal(err)
	}
	cmd, out := newPromptCmd("")

	if err := runDisableWorktree(cmd, repo); err != nil {
		t.Fatalf("runDisableWorktree: %v", err)
	}
	if !strings.Contains(out.String(), "disable --repo") {
		t.Errorf("expected a heads-up pointing at `disable --repo`, got %q", out.String())
	}
}
