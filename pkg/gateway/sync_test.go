package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/storage"
)

// newSyncGateway builds a gateway backed by a real in-memory sync server
// (SQLite store + Memory storage) and a stub provisioner, plus an HTTP
// test server with a fixed "tester" principal.
func newSyncGateway(t *testing.T) (*httptest.Server, *clanksync.Server, *stubProvisioner) {
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
	prov := &stubProvisioner{ref: provisioner.HostRef{URL: "http://127.0.0.1:1"}}
	g, err := NewGateway(Config{Provisioner: prov, Sync: syncSrv}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	srv := httptest.NewServer(localAuth(g.Handler(), "tester"))
	t.Cleanup(srv.Close)
	return srv, syncSrv, prov
}

// TestSyncAll_NoCheckpoint verifies sync-all wakes the sprite once and
// reports no_checkpoint for a worktree the laptop never pushed — without
// ever dialing the (bogus) sprite URL, since the routine short-circuits
// before any apply.
func TestSyncAll_NoCheckpoint(t *testing.T) {
	srv, syncSrv, prov := newSyncGateway(t)
	if err := syncSrv.RegisterPrebuiltWorktree(context.Background(), clanksync.Worktree{
		ID: "wt1", UserID: "tester", DisplayName: "demo",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(srv.URL+"/v1/worktrees/sync", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var out syncAllResponse
	if err := jsonDecode(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].State != syncStateNoCheckpoint {
		t.Fatalf("results=%+v, want one no_checkpoint", out.Results)
	}
	if out.Synced != 0 || out.Conflicts != 0 {
		t.Errorf("synced=%d conflicts=%d, want 0/0", out.Synced, out.Conflicts)
	}
	if prov.ensureCalls != 1 {
		t.Errorf("EnsureHost calls=%d, want 1 (sync-all wakes the sprite once)", prov.ensureCalls)
	}
}

// TestSyncWorktree_NoCheckpoint covers the per-worktree route (manual
// sync button) returning the single outcome for an unpushed worktree.
func TestSyncWorktree_NoCheckpoint(t *testing.T) {
	srv, syncSrv, _ := newSyncGateway(t)
	if err := syncSrv.RegisterPrebuiltWorktree(context.Background(), clanksync.Worktree{
		ID: "wtX", UserID: "tester", DisplayName: "demo",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(srv.URL+"/v1/worktrees/wtX/sync", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var out syncResult
	if err := jsonDecode(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.WorktreeID != "wtX" || out.State != syncStateNoCheckpoint {
		t.Fatalf("result=%+v, want wtX/no_checkpoint", out)
	}
}

// TestSyncWorktree_MalformedBody verifies that a malformed JSON body returns 400
// rather than silently defaulting force to false.
func TestSyncWorktree_MalformedBody(t *testing.T) {
	t.Parallel()
	srv, syncSrv, _ := newSyncGateway(t)
	if err := syncSrv.RegisterPrebuiltWorktree(context.Background(), clanksync.Worktree{
		ID: "wtMalformed", UserID: "tester", DisplayName: "demo",
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/v1/worktrees/wtMalformed/sync", "application/json",
		strings.NewReader(`{bad json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for malformed body", resp.StatusCode)
	}
}

// TestSyncWorktree_UnknownWorktree covers tenancy/missing handling: a
// worktree id that doesn't exist returns a 4xx (GetWorktree fails before
// any sprite work).
func TestSyncWorktree_UnknownWorktree(t *testing.T) {
	srv, _, _ := newSyncGateway(t)
	resp, err := http.Post(srv.URL+"/v1/worktrees/nope/sync", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status 200, want a 4xx for an unknown worktree")
	}
}
