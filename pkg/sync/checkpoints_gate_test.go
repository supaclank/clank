package sync_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	clanksync "github.com/acksell/clank/pkg/sync"
)

// TestCreateCheckpoint_LocalCallerAllowedOnRemoteOwnedWorktree pins the
// push-only gate relaxation: a laptop (local caller) may push a
// checkpoint to its own worktree even while a remote sprite currently
// owns it. The laptop is the only writer of the checkpoint slot, so
// there is no contention and no ownership flip.
func TestCreateCheckpoint_LocalCallerAllowedOnRemoteOwnedWorktree(t *testing.T) {
	t.Parallel()
	httpSrv, store, _ := newTestServer(t)

	if err := store.InsertWorktree(context.Background(), clanksync.Worktree{
		ID:        "wt-remote-owned",
		UserID:    "user-A",
		OwnerKind: clanksync.OwnerKindRemote,
		OwnerID:   "sprite-1",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertWorktree: %v", err)
	}

	create := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints", map[string]string{
		"worktree_id":        "wt-remote-owned",
		"head_commit":        "deadbeef",
		"index_tree":         "1111",
		"worktree_tree":      "2222",
		"incremental_commit": "3333",
	})
	if id, _ := create["checkpoint_id"].(string); id == "" {
		t.Fatalf("local push to a remote-owned worktree should succeed, got %v", create)
	}
}

// TestCreateCheckpoint_CrossTenantStillForbidden confirms the gate
// relaxation did not weaken tenancy: a caller still cannot push to a
// worktree owned by a different user.
func TestCreateCheckpoint_CrossTenantStillForbidden(t *testing.T) {
	t.Parallel()
	httpSrv, store, _ := newTestServer(t)

	if err := store.InsertWorktree(context.Background(), clanksync.Worktree{
		ID:        "wt-other-user",
		UserID:    "user-B",
		OwnerKind: clanksync.OwnerKindLocal,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertWorktree: %v", err)
	}

	mustPostExpectStatus(t, httpSrv.URL+"/v1/checkpoints", map[string]string{
		"worktree_id":        "wt-other-user",
		"head_commit":        "deadbeef",
		"index_tree":         "1111",
		"worktree_tree":      "2222",
		"incremental_commit": "3333",
	}, http.StatusForbidden)
}
