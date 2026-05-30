package sync_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	clanksync "github.com/acksell/clank/pkg/sync"
)

// TestCreateCheckpoint_CrossTenantForbidden pins the only authorization
// the checkpoint path enforces now that ownership is gone: tenancy. A
// caller may push to any of its own worktrees, but never to one owned
// by a different user.
func TestCreateCheckpoint_CrossTenantForbidden(t *testing.T) {
	t.Parallel()
	httpSrv, store, _ := newTestServer(t)

	if err := store.InsertWorktree(context.Background(), clanksync.Worktree{
		ID:        "wt-other-user",
		UserID:    "user-B",
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
