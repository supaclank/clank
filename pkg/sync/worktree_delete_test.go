package sync_test

import (
	"context"
	"errors"
	"testing"
	"time"

	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/storage"
)

// newDeleteServer builds a sync Server over an in-memory store for
// unit-testing the DeleteWorktree service method directly.
func newDeleteServer(t *testing.T) (*clanksync.Server, *memSyncStore) {
	t.Helper()
	store := newMemSyncStore()
	mem := storage.NewMemory()
	t.Cleanup(mem.Close)
	srv, err := clanksync.NewServer(clanksync.Config{Store: store, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, store
}

func seedWorktreeWithCheckpoints(t *testing.T, store *memSyncStore, id, userID string, n int) {
	t.Helper()
	ctx := context.Background()
	if err := store.InsertWorktree(ctx, clanksync.Worktree{ID: id, UserID: userID, DisplayName: "demo"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ckID := id + "-ck-" + string(rune('a'+i))
		if err := store.InsertCheckpoint(ctx, clanksync.Checkpoint{ID: ckID, WorktreeID: id}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDeleteWorktree_CascadesCheckpoints: the owner can delete a worktree and
// its checkpoint rows go with it (checkpoints have no FK cascade, so the
// service must clear them explicitly — otherwise orphans accrue).
func TestDeleteWorktree_CascadesCheckpoints(t *testing.T) {
	t.Parallel()
	srv, store := newDeleteServer(t)
	ctx := context.Background()
	seedWorktreeWithCheckpoints(t, store, "w1", "user-A", 3)

	if err := srv.DeleteWorktree(ctx, "user-A", "w1"); err != nil {
		t.Fatalf("DeleteWorktree: %v", err)
	}
	if _, err := store.GetWorktreeByID(ctx, "w1"); !errors.Is(err, clanksync.ErrWorktreeNotFound) {
		t.Fatalf("worktree still present: err=%v", err)
	}
	cps, err := store.ListCheckpointsByWorktree(ctx, "w1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 0 {
		t.Fatalf("checkpoints=%d after delete, want 0 (orphans left behind)", len(cps))
	}
}

// TestDeleteWorktree_Tenancy: a non-owner can't delete the worktree, and the
// row is left intact.
func TestDeleteWorktree_Tenancy(t *testing.T) {
	t.Parallel()
	srv, store := newDeleteServer(t)
	ctx := context.Background()
	seedWorktreeWithCheckpoints(t, store, "w1", "user-A", 1)

	if err := srv.DeleteWorktree(ctx, "user-B", "w1"); !errors.Is(err, clanksync.ErrForbidden) {
		t.Fatalf("DeleteWorktree as non-owner: err=%v, want ErrForbidden", err)
	}
	if _, err := store.GetWorktreeByID(ctx, "w1"); err != nil {
		t.Fatalf("worktree deleted despite tenancy failure: %v", err)
	}
}

// TestDeleteWorktree_Missing: deleting an unknown id reports not-found.
func TestDeleteWorktree_Missing(t *testing.T) {
	t.Parallel()
	srv, _ := newDeleteServer(t)
	if err := srv.DeleteWorktree(context.Background(), "user-A", "ghost"); !errors.Is(err, clanksync.ErrWorktreeNotFound) {
		t.Fatalf("DeleteWorktree(ghost): err=%v, want ErrWorktreeNotFound", err)
	}
}
