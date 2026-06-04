package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/storage"
)

const delUser = "tester"

// deletingSprite is an in-process stand-in for the clank-host sprite's
// DELETE /worktrees/{id} (materialized-cleanup) endpoint. It counts calls and
// returns a configurable status so a test can drive the gateway's strict
// ordering (host cleanup must succeed before the sync row is deleted).
type deletingSprite struct {
	status      int // response for DELETE /worktrees/{id}; 0 ⇒ 204
	deleteCalls int32
}

func (ds *deletingSprite) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /worktrees/{id}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ds.deleteCalls, 1)
		if ds.status == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(ds.status)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newDeleteGateway builds a gateway whose provisioner points at the given
// sprite, backed by a real SQLite sync store. Mirrors newMaterializeGateway.
func newDeleteGateway(t *testing.T, sprite *httptest.Server) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mem := storage.NewMemory()
	t.Cleanup(mem.Close)
	syncSrv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	prov := &stubProvisioner{ref: provisioner.HostRef{URL: sprite.URL}}
	g, err := NewGateway(Config{Provisioner: prov, Sync: syncSrv}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(localAuth(g.Handler(), delUser))
	t.Cleanup(gw.Close)
	return gw, st
}

func seedDeletableWorktree(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.InsertWorktree(ctx, clanksync.Worktree{
		ID: id, UserID: delUser, DisplayName: "demo", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertCheckpoint(ctx, clanksync.Checkpoint{
		ID: id + "-ck", WorktreeID: id, HeadCommit: "h", IndexTree: "i",
		WorktreeTree: "w", UncommittedCommit: "u", CreatedAt: now, CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}
}

func httpDelete(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func worktreePresent(t *testing.T, st *store.Store, id string) bool {
	t.Helper()
	_, err := st.GetWorktreeByID(context.Background(), id)
	return err == nil
}

// TestDeleteWorktree_HappyPath: sprite cleanup succeeds (204), so the gateway
// deletes the sync row + its checkpoint rows and returns 204.
func TestDeleteWorktree_HappyPath(t *testing.T) {
	t.Parallel()
	sprite := &deletingSprite{status: http.StatusNoContent}
	gw, st := newDeleteGateway(t, sprite.server(t))
	seedDeletableWorktree(t, st, "wt-del")

	resp := httpDelete(t, gw.URL+"/v1/worktrees/wt-del")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&sprite.deleteCalls); n != 1 {
		t.Fatalf("sprite delete calls=%d, want 1", n)
	}
	if worktreePresent(t, st, "wt-del") {
		t.Fatalf("worktree row still present after delete")
	}
	cps, err := st.ListCheckpointsByWorktree(context.Background(), "wt-del", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 0 {
		t.Fatalf("checkpoints=%d after delete, want 0 (orphans)", len(cps))
	}
}

// TestDeleteWorktree_HostFailureAborts: a sprite 500 aborts the whole delete
// (502) and leaves the sync row intact for a retry.
func TestDeleteWorktree_HostFailureAborts(t *testing.T) {
	t.Parallel()
	sprite := &deletingSprite{status: http.StatusInternalServerError}
	gw, st := newDeleteGateway(t, sprite.server(t))
	seedDeletableWorktree(t, st, "wt-keep")

	resp := httpDelete(t, gw.URL+"/v1/worktrees/wt-keep")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 on sprite failure", resp.StatusCode)
	}
	if !worktreePresent(t, st, "wt-keep") {
		t.Fatalf("worktree row deleted despite sprite failure (not strict)")
	}
}

// TestDeleteWorktree_BusyMaps409: a sprite 409 (active session) surfaces to the
// client as a 409, with the row left intact.
func TestDeleteWorktree_BusyMaps409(t *testing.T) {
	t.Parallel()
	sprite := &deletingSprite{status: http.StatusConflict}
	gw, st := newDeleteGateway(t, sprite.server(t))
	seedDeletableWorktree(t, st, "wt-busy")

	resp := httpDelete(t, gw.URL+"/v1/worktrees/wt-busy")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409 for a busy worktree", resp.StatusCode)
	}
	if !worktreePresent(t, st, "wt-busy") {
		t.Fatalf("worktree row deleted despite busy")
	}
}

// TestDeleteWorktree_UnknownReturns404: an unknown id fails tenancy before any
// sprite call (the gateway never dials the host).
func TestDeleteWorktree_UnknownReturns404(t *testing.T) {
	t.Parallel()
	sprite := &deletingSprite{status: http.StatusNoContent}
	gw, _ := newDeleteGateway(t, sprite.server(t))

	resp := httpDelete(t, gw.URL+"/v1/worktrees/ghost")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 for unknown worktree", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&sprite.deleteCalls); n != 0 {
		t.Fatalf("sprite delete calls=%d, want 0 (tenancy fails before sprite)", n)
	}
}
