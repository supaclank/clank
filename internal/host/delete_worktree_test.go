package host_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/host/store"
	"github.com/oklog/ulid/v2"
)

func newDeleteHostService(t *testing.T) (*host.Service, *store.Store, string) {
	t.Helper()
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)
	return svc, st, workRoot
}

func seedHostSession(t *testing.T, st *store.Store, id, worktreeID string, status agent.SessionStatus) {
	t.Helper()
	now := time.Now()
	if err := st.UpsertSession(context.Background(), agent.SessionInfo{
		ID: id, Backend: agent.BackendOpenCode, Status: status,
		GitRef: agent.GitRef{WorktreeID: worktreeID}, Prompt: "p",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDeleteWorktree_RemovesDirAndSessions: an idle worktree is
// fully cleaned — its ~/work/<id> directory and all its sessions are gone —
// while a session on a *different* worktree is left untouched.
func TestDeleteWorktree_RemovesDirAndSessions(t *testing.T) {
	svc, st, workRoot := newDeleteHostService(t)
	ctx := context.Background()

	wtDel := ulid.Make().String()
	wtOther := ulid.Make().String()
	sDel := ulid.Make().String()
	sKeep := ulid.Make().String()

	dir := filepath.Join(workRoot, wtDel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedHostSession(t, st, sDel, wtDel, agent.StatusIdle)
	seedHostSession(t, st, sKeep, wtOther, agent.StatusIdle)

	if err := svc.DeleteWorktree(ctx, wtDel); err != nil {
		t.Fatalf("DeleteWorktree: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("~/work/%s still present: err=%v", wtDel, err)
	}
	gone, err := st.ListSessionsByWorktree(ctx, wtDel)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 0 {
		t.Fatalf("sessions for deleted worktree=%d, want 0", len(gone))
	}
	kept, err := st.ListSessionsByWorktree(ctx, wtOther)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Fatalf("sessions for other worktree=%d, want 1 (collateral delete)", len(kept))
	}
}

// TestDeleteWorktree_RefusesWhenBusy: a running session blocks the
// delete (ErrWorktreeBusy) and nothing is removed.
func TestDeleteWorktree_RefusesWhenBusy(t *testing.T) {
	svc, st, workRoot := newDeleteHostService(t)
	ctx := context.Background()

	wtBusy := ulid.Make().String()
	sBusy := ulid.Make().String()

	dir := filepath.Join(workRoot, wtBusy)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedHostSession(t, st, sBusy, wtBusy, agent.StatusBusy)

	if err := svc.DeleteWorktree(ctx, wtBusy); !errors.Is(err, host.ErrWorktreeBusy) {
		t.Fatalf("DeleteWorktree on busy worktree: err=%v, want ErrWorktreeBusy", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir removed despite busy session: %v", err)
	}
	left, err := st.ListSessionsByWorktree(ctx, wtBusy)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Fatalf("sessions removed despite busy: got %d, want 1", len(left))
	}
}

// TestDeleteWorktree_NeverMaterialized: a worktree with no dir and
// no sessions is a clean no-op (idempotent — supports gateway retry).
func TestDeleteWorktree_NeverMaterialized(t *testing.T) {
	svc, _, _ := newDeleteHostService(t)
	if err := svc.DeleteWorktree(context.Background(), ulid.Make().String()); err != nil {
		t.Fatalf("DeleteWorktree on a never-created worktree: %v", err)
	}
}
