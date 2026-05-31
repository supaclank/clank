package clankcli

import (
	"context"
	"io"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/clanksync/triggers"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/store"
	clanksync "github.com/acksell/clank/pkg/sync"
	syncclient "github.com/acksell/clank/pkg/sync/client"
	"github.com/acksell/clank/pkg/sync/storage"
)

// newSyncServer spins up a real sync server (sqlite + in-memory storage)
// behind an httptest endpoint with a fixed principal, returning a client
// pointed at it. No mocks — the same harness the syncclient tests use.
func newSyncServer(t *testing.T) (*syncclient.Client, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mem := storage.NewMemory()
	t.Cleanup(func() { mem.Close() })
	srv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(withFixedPrincipal("user-A", srv.Handler()))
	t.Cleanup(httpSrv.Close)
	cli, err := syncclient.New(syncclient.Config{BaseURL: httpSrv.URL, AuthToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return cli, st
}

// newGitRepo creates a one-commit git repo in a temp dir.
func newGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	pgit(t, repo, "init", "-q")
	pgit(t, repo, "config", "user.email", "t@e.com")
	pgit(t, repo, "config", "user.name", "t")
	pWrite(t, filepath.Join(repo, "f.txt"), "v1")
	pgit(t, repo, "add", ".")
	pgit(t, repo, "commit", "-qm", "c1")
	return repo
}

// TestEnsureTracked_NonInteractiveUntrackedErrors pins the autopush-hook
// safety path: an untracked repo with no global auto-push and no TTY must
// error WITHOUT reading stdin (a prompt would hang the hook).
func TestEnsureTracked_NonInteractiveUntrackedErrors(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir()) // no AutoPushAllRepos
	repo := newGitRepo(t)
	cmd, _ := newPromptCmd("y\n") // input present; must NOT be consumed

	_, err := ensureTracked(context.Background(), cmd, nil, repo, "", false)
	if err == nil {
		t.Fatal("untracked + non-interactive must error, not prompt")
	}
	rest, _ := io.ReadAll(cmd.InOrStdin())
	if string(rest) != "y\n" {
		t.Errorf("stdin was consumed (%q left); non-interactive path must not read input", rest)
	}
}

// TestEnsureTracked_InteractivePromptRegisters drives the TTY path: a
// "yes" to the track prompt registers the worktree and caches the id.
// SyncHarnesses is pre-set so the harness multiselect (a TUI widget that
// needs a real terminal) is skipped — its logic is covered by the
// harnessSelectModel tests.
func TestEnsureTracked_InteractivePromptRegisters(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))
	if err := config.UpdatePreferences(func(p *config.Preferences) {
		p.SyncHarnesses = []string{triggers.HarnessClaudeCode, triggers.HarnessOpenCode}
	}); err != nil {
		t.Fatal(err)
	}
	cli, st := newSyncServer(t)
	repo := newGitRepo(t)
	cmd, _ := newPromptCmd("y\n") // track? yes

	ctx := context.Background()
	id, err := ensureTracked(ctx, cmd, cli, repo, "", true)
	if err != nil {
		t.Fatalf("ensureTracked: %v", err)
	}
	if id == "" {
		t.Fatal("empty worktree id")
	}
	cached, _ := agent.ReadLocalWorktreeID(repo)
	if cached != id {
		t.Fatalf("on-disk id %q != returned %q", cached, id)
	}
	if _, err := st.GetWorktreeByID(ctx, id); err != nil {
		t.Fatalf("worktree not registered on remote: %v", err)
	}
}

// TestEnsureTracked_AutoPushRegistersWithoutPrompt pins that
// AutoPushAllRepos auto-registers even non-interactively, without
// touching stdin.
func TestEnsureTracked_AutoPushRegistersWithoutPrompt(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := config.UpdatePreferences(func(p *config.Preferences) {
		p.AutoPushAllRepos = true
	}); err != nil {
		t.Fatal(err)
	}
	cli, st := newSyncServer(t)
	repo := newGitRepo(t)
	cmd, _ := newPromptCmd("y\n") // must NOT be consumed

	ctx := context.Background()
	id, err := ensureTracked(ctx, cmd, cli, repo, "", false)
	if err != nil {
		t.Fatalf("ensureTracked: %v", err)
	}
	if id == "" {
		t.Fatal("empty worktree id")
	}
	if _, err := st.GetWorktreeByID(ctx, id); err != nil {
		t.Fatalf("worktree not registered: %v", err)
	}
	rest, _ := io.ReadAll(cmd.InOrStdin())
	if string(rest) != "y\n" {
		t.Errorf("stdin consumed (%q left); auto-push path must not prompt", rest)
	}
}

// TestEnsureLoggedIn_ExplicitCredsSkipLogin pins that explicit
// --base-url + --token (self-hosted static bearer / CI) yield a client
// without any login flow or network call.
func TestEnsureLoggedIn_ExplicitCredsSkipLogin(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	cmd, _ := newPromptCmd("") // non-interactive
	cli, err := ensureLoggedIn(context.Background(), cmd, "https://gw.example.com", "static-bearer")
	if err != nil {
		t.Fatalf("ensureLoggedIn with explicit creds: %v", err)
	}
	if cli == nil {
		t.Fatal("nil client")
	}
}

// TestEnsureLoggedIn_NonInteractiveSignedOutErrors pins that a signed-out
// non-TTY caller gets a clean error rather than a prompt.
func TestEnsureLoggedIn_NonInteractiveSignedOutErrors(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir()) // no remote configured
	cmd, _ := newPromptCmd("")
	if _, err := ensureLoggedIn(context.Background(), cmd, "", ""); err == nil {
		t.Fatal("signed-out + non-interactive must error")
	}
}

// TestHintRepoTracking pins the returning-user nudge: an untracked git
// repo gets a one-line hint; tracked / auto-push / non-repo stay silent
// (so `clank login` doesn't re-run onboarding it already did).
func TestHintRepoTracking(t *testing.T) {
	t.Parallel()

	t.Run("untracked repo nudges", func(t *testing.T) {
		t.Parallel()
		repo := newGitRepo(t)
		cmd, out := newPromptCmd("")
		if err := hintRepoTracking(cmd, config.Preferences{}, repo); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "isn't tracked yet") {
			t.Errorf("want untracked nudge, got %q", out.String())
		}
	})

	t.Run("tracked repo stays silent", func(t *testing.T) {
		t.Parallel()
		repo := newGitRepo(t)
		if err := agent.WriteLocalWorktreeID(repo, "wt-already"); err != nil {
			t.Fatal(err)
		}
		cmd, out := newPromptCmd("")
		if err := hintRepoTracking(cmd, config.Preferences{}, repo); err != nil {
			t.Fatal(err)
		}
		if out.String() != "" {
			t.Errorf("tracked repo must not nudge, got %q", out.String())
		}
	})

	t.Run("auto-push stays silent", func(t *testing.T) {
		t.Parallel()
		repo := newGitRepo(t)
		cmd, out := newPromptCmd("")
		if err := hintRepoTracking(cmd, config.Preferences{AutoPushAllRepos: true}, repo); err != nil {
			t.Fatal(err)
		}
		if out.String() != "" {
			t.Errorf("auto-push must not nudge, got %q", out.String())
		}
	})

	t.Run("non-repo stays silent", func(t *testing.T) {
		t.Parallel()
		cmd, out := newPromptCmd("")
		if err := hintRepoTracking(cmd, config.Preferences{}, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if out.String() != "" {
			t.Errorf("non-repo must not nudge, got %q", out.String())
		}
	})
}

// TestLoginOnboarding_NonInteractiveNoOp pins that a scripted login does
// no onboarding and reads no stdin.
func TestLoginOnboarding_NonInteractiveNoOp(t *testing.T) {
	t.Parallel()
	cmd, out := newPromptCmd("y\n")
	if err := loginOnboarding(cmd); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Errorf("non-interactive login must not prompt, got %q", out.String())
	}
	rest, _ := io.ReadAll(cmd.InOrStdin())
	if string(rest) != "y\n" {
		t.Errorf("stdin consumed (%q left)", rest)
	}
}
