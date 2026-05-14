package sync_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/storage"
)

// TestSessionPresignHandler_HappyPath: laptop creates a checkpoint
// over HTTP, then asks the new /sessions endpoint for upload URLs.
// Verifies the wire shape and that the URLs are addressable.
func TestSessionPresignHandler_HappyPath(t *testing.T) {
	t.Parallel()
	httpSrv, _, mem := newTestServer(t)

	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "myrepo",
	})
	worktreeID := wt["id"].(string)

	create := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints", map[string]string{
		"worktree_id":        worktreeID,
		"head_commit":        "deadbeef",
		"head_ref":           "main",
		"index_tree":         "1111",
		"worktree_tree":      "2222",
		"incremental_commit": "3333",
	})
	checkpointID := create["checkpoint_id"].(string)

	resp := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints/"+checkpointID+"/sessions", map[string]any{
		"session_ids": []string{"01HSESSA", "01HSESSB"},
	})
	if resp["checkpoint_id"] != checkpointID {
		t.Errorf("checkpoint_id = %v, want %v", resp["checkpoint_id"], checkpointID)
	}
	if resp["session_manifest_put_url"].(string) == "" {
		t.Errorf("missing session_manifest_put_url")
	}
	urls, ok := resp["session_put_urls"].(map[string]any)
	if !ok {
		t.Fatalf("session_put_urls is %T, want map", resp["session_put_urls"])
	}
	if len(urls) != 2 {
		t.Errorf("want 2 session URLs, got %d", len(urls))
	}
	if urls["01HSESSA"].(string) == "" || urls["01HSESSB"].(string) == "" {
		t.Errorf("empty per-session URLs: %v", urls)
	}

	// Upload to one of the URLs and verify it lands at the expected key.
	uploadTo(t, urls["01HSESSA"].(string), []byte(`{"info":{"id":"ses_a"}}`))
	wantKey := "checkpoints/user-A/" + worktreeID + "/" + checkpointID + "/sessions/01HSESSA.json"
	var found bool
	for _, k := range mem.Keys() {
		if k == wantKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("upload landed at unexpected key; want %s, have %v", wantKey, mem.Keys())
	}
}

// TestSessionPresignHandler_EmptySessions: requesting URLs for zero
// sessions still returns a manifest URL.
func TestSessionPresignHandler_EmptySessions(t *testing.T) {
	t.Parallel()
	httpSrv, _, _ := newTestServer(t)

	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "myrepo",
	})
	create := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints", map[string]string{
		"worktree_id":        wt["id"].(string),
		"head_commit":        "deadbeef",
		"head_ref":           "main",
		"index_tree":         "1111",
		"worktree_tree":      "2222",
		"incremental_commit": "3333",
	})
	checkpointID := create["checkpoint_id"].(string)

	resp := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints/"+checkpointID+"/sessions", map[string]any{
		"session_ids": []string{},
	})
	if resp["session_manifest_put_url"].(string) == "" {
		t.Errorf("manifest URL should be set even for empty session_ids")
	}
}

// TestSessionPresignHandler_UnknownCheckpointReturns404 pins error mapping.
func TestSessionPresignHandler_UnknownCheckpointReturns404(t *testing.T) {
	t.Parallel()
	httpSrv, _, _ := newTestServer(t)

	resp := mustPostExpectStatus(t, httpSrv.URL+"/v1/checkpoints/ck-does-not-exist/sessions", map[string]any{
		"session_ids": []string{"01H"},
	}, http.StatusNotFound)
	if !strings.Contains(string(resp), "checkpoint not found") {
		t.Errorf("expected 'checkpoint not found' in body, got %q", resp)
	}
}

// TestSessionPresignHandler_WrongTenantForbidden: a checkpoint
// belonging to a different user gets 403, not "not found", so the
// client can distinguish "you can't see this" from "doesn't exist".
func TestSessionPresignHandler_WrongTenantForbidden(t *testing.T) {
	t.Parallel()

	// Build a server with TWO users — easiest path is to spin up
	// directly via NewServer so we can sidestep the fixed-principal
	// middleware that all the existing tests pin to "user-A".
	store := newMemSyncStore()
	mem := storage.NewMemory()
	t.Cleanup(mem.Close)

	srv, err := clanksync.NewServer(clanksync.Config{
		Store:      store,
		Storage:    mem,
		PresignTTL: time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Seed a worktree + checkpoint belonging to user-A.
	now := time.Now().UTC()
	if err := store.InsertWorktree(context.Background(), clanksync.Worktree{
		ID: "wt-A", UserID: "user-A",
		OwnerKind: clanksync.OwnerKindLocal, OwnerID: "",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCheckpoint(context.Background(), clanksync.Checkpoint{
		ID: "ck-A", WorktreeID: "wt-A",
		HeadCommit: "deadbeef", IndexTree: "1111",
		WorktreeTree: "2222", IncrementalCommit: "3333",
		CreatedAt: now, CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// user-B asks for presigned URLs against user-A's checkpoint.
	_, err = srv.PresignSessionPuts(context.Background(), "user-B", clanksync.SessionPresignRequest{
		CheckpointID: "ck-A",
		SessionIDs:   []string{"01H"},
	})
	if err == nil {
		t.Fatal("expected forbidden error")
	}
}
