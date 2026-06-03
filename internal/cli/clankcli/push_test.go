package clankcli

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/sessionsync"
)

// withFixedPrincipal injects a fixed Principal so every request resolves
// to the same UserID — stand-in for real auth in tests.
func withFixedPrincipal(userID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), auth.Principal{UserID: userID})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TestRunPush_SelfHealsStaleWorktreeID pins the 404 self-heal: when the
// cached worktree-id points at a row the remote no longer has (deleted
// upstream), runPush re-registers, rewrites the on-disk id, and retries
// the push — replacing the old `rm -r .git/clank && clank init` recovery.
func TestRunPush_SelfHealsStaleWorktreeID(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cli, st := newSyncServer(t)
	repo := newGitRepo(t)

	// Stale cached id — points at a worktree the remote never had.
	const staleID = "wt-stale-deleted-upstream"
	if err := agent.WriteLocalWorktreeID(repo, staleID); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	cmd.SetContext(ctx) // pushSessionLeg derives its deadline from cmd.Context()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	// InSync=false drives the push; the create 404s on the stale id and
	// runPush must recover rather than surface the error.
	parity := parityResult{InSync: false, RemoteNotFound: true}
	if err := runPush(cmd, ctx, newPhaseTimer(false), cli, repo, staleID, parity, false); err != nil {
		t.Fatalf("runPush should self-heal the stale id, got: %v", err)
	}

	// The on-disk id must now be a freshly-registered one.
	got, err := agent.ReadLocalWorktreeID(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || got == staleID {
		t.Fatalf("worktree id not re-registered: got %q (stale %q)", got, staleID)
	}
	// And the remote must hold that worktree with a committed checkpoint.
	wt, err := st.GetWorktreeByID(ctx, got)
	if err != nil {
		t.Fatalf("re-registered worktree missing on remote: %v", err)
	}
	if wt.LatestSyncedCheckpoint == "" {
		t.Fatal("push did not commit a checkpoint after self-heal")
	}
}

// TestRunPush_SyncsUnsyncedSessionWhenCodeInSync is the regression test for
// the stuck-session bug: code parity ignores sessions, so a session that
// changed without any code change (e.g. a Claude transcript whose mtime
// bumped on a bare `--resume`) must still be carried. Before the fix,
// runPush early-returned "✓ Already up to date" on code parity and never
// pushed the session, so `clank status` flagged it forever.
//
// Not parallel: isolates CLAUDE_CONFIG_DIR via t.Setenv.
func TestRunPush_SyncsUnsyncedSessionWhenCodeInSync(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cli, st := newSyncServer(t)
	repo := newGitRepo(t)

	id, err := cli.RegisterWorktree(ctx, "myrepo", "")
	if err != nil {
		t.Fatalf("register worktree: %v", err)
	}
	if err := agent.WriteLocalWorktreeID(repo, id); err != nil {
		t.Fatal(err)
	}

	// First push: commit a checkpoint (no sessions yet) so code is in sync.
	if err := runPush(quietCmd(ctx), ctx, newPhaseTimer(false), cli, repo, id, parityResult{InSync: false}, false); err != nil {
		t.Fatalf("first push: %v", err)
	}
	wt, err := st.GetWorktreeByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	ckID := wt.LatestSyncedCheckpoint
	if ckID == "" {
		t.Fatal("first push did not commit a checkpoint")
	}

	// Seed an unsynced Claude session: install a transcript under the repo's
	// encoded path. The first push recorded no sessions, so this one is
	// unsynced relative to the last-pushed record.
	const sessionID = "push-claude-sess-1"
	blob := []byte(`{"type":"user","sessionId":"` + sessionID + `","cwd":"/orig","gitBranch":"main","message":{"role":"user","content":"hi"}}` + "\n")
	blobPath := filepath.Join(t.TempDir(), "seed.jsonl")
	if err := os.WriteFile(blobPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionsync.ImportSessionBlob(ctx, agent.BackendClaudeCode, blobPath, repo); err != nil {
		t.Fatalf("seed claude session: %v", err)
	}

	// Second push: code is in sync, but the session is not — it MUST be
	// carried, not short-circuited as "up to date".
	out := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := runPush(cmd, ctx, newPhaseTimer(false), cli, repo, id, parityResult{InSync: true, HasCheckpoint: true, CheckpointID: ckID}, false); err != nil {
		t.Fatalf("second push: %v", err)
	}

	if strings.Contains(out.String(), "Already up to date") {
		t.Errorf("push reported up-to-date despite an unsynced session:\n%s", out.String())
	}

	// The session must now be in the last-pushed record (proves the session
	// leg ran against the existing checkpoint).
	rec, err := agent.ReadSyncedSessions(repo)
	if err != nil {
		t.Fatalf("read synced sessions: %v", err)
	}
	if _, ok := rec.Sessions[sessionID]; !ok {
		t.Fatalf("session %s not in synced record after push; record=%+v", sessionID, rec.Sessions)
	}
	if rec.LastPushedAt.IsZero() {
		t.Error("push did not stamp LastPushedAt on the record")
	}
}

func quietCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd
}
