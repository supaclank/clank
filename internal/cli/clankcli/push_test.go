package clankcli

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/auth"
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
	if err := runPush(cmd, ctx, newPhaseTimer(false), cli, repo, staleID, parity); err != nil {
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
