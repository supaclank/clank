package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/checkpoint"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// --- session-leg materialization (delta-only autosync) -----------------

const (
	matUser = "tester"
	matWT   = "wt-mat"
	matCK   = "ck-mat"
	matHead = "headcommit000000000000000000000000000000"
	// Two distinct 64-hex session content hashes.
	hashA = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	hashB = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
)

// recordingSprite is an in-process stand-in for the clank-host sprite: it
// answers /sync/apply-from-urls with a fixed apply state and counts calls to
// /sync/sessions/apply-from-urls so a test can assert whether the gateway
// drove a session import. It's a real HTTP server (not a Go-level mock) — the
// gateway dials it exactly as it would a remote sprite.
type recordingSprite struct {
	applyState   string
	sessionCalls int32
}

func (rs *recordingSprite) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sync/apply-from-urls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"state": rs.applyState})
	})
	mux.HandleFunc("POST /sync/sessions/apply-from-urls", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rs.sessionCalls, 1)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newMaterializeGateway builds a gateway whose provisioner points at the
// given sprite, backed by a real SQLite sync store + Memory storage. Returns
// the gateway test server, the underlying store (for seeding/assertions), and
// the sync server (for minting presigned session-manifest URLs).
func newMaterializeGateway(t *testing.T, sprite *httptest.Server) (*httptest.Server, *store.Store, *clanksync.Server) {
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
	gw := httptest.NewServer(localAuth(g.Handler(), matUser))
	t.Cleanup(gw.Close)
	return gw, st, syncSrv
}

// seedMaterializedWorktree creates a worktree that has already been
// fast-forwarded onto the sprite: an uploaded checkpoint, a baseline head
// bundle (so DownloadCheckpointURLs resolves), and sync_state=up_to_date with
// an empty session digest. This is the steady state a session-only push lands
// in — code current, only sessions changed.
func seedMaterializedWorktree(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.InsertWorktree(ctx, clanksync.Worktree{
		ID: matWT, UserID: matUser, DisplayName: "demo",
		LatestSyncedCheckpoint: matCK, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertCheckpoint(ctx, clanksync.Checkpoint{
		ID: matCK, WorktreeID: matWT, HeadCommit: matHead,
		IndexTree: "i", WorktreeTree: "w", UncommittedCommit: "u",
		CreatedAt: now, CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkCheckpointUploaded(ctx, matCK, now); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertHeadBundle(ctx, clanksync.HeadBundle{
		UserID: matUser, TipSHA: matHead, BlobKey: "blob/head", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateWorktreeMaterialization(ctx, matWT, clanksync.MaterializationUpdate{
		MaterializedCheckpointID: matCK, SyncState: clanksync.SyncStateUpToDate,
	}); err != nil {
		t.Fatal(err)
	}
}

// putSessionManifest uploads a session manifest for matCK to object storage
// via a presigned PUT (the same key the gateway later reads) and returns its
// content-digest.
func putSessionManifest(t *testing.T, syncSrv *clanksync.Server, entries []checkpoint.SessionEntry) string {
	t.Helper()
	ctx := context.Background()
	refs := make([]checkpoint.SessionBlobRef, len(entries))
	for i, e := range entries {
		refs[i] = e.BlobRef()
	}
	pres, err := syncSrv.PresignSessionPuts(ctx, matUser, clanksync.SessionPresignRequest{
		CheckpointID: matCK, Sessions: refs,
	})
	if err != nil {
		t.Fatal(err)
	}
	man := &checkpoint.SessionManifest{
		Version: checkpoint.SessionManifestVersion, CheckpointID: matCK,
		CreatedAt: time.Now().UTC(), CreatedBy: "laptop", Sessions: entries,
	}
	data, err := man.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, pres.SessionManifestPutURL, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT manifest: status %d", resp.StatusCode)
	}
	return man.ContentDigest()
}

// syncWorktreeOnce drives POST /v1/worktrees/{id}/sync and returns the
// decoded outcome, failing on a non-200 or a session-import error in Detail.
func syncWorktreeOnce(t *testing.T, gw *httptest.Server) syncResult {
	t.Helper()
	resp, err := http.Post(gw.URL+"/v1/worktrees/"+matWT+"/sync", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync status %d, want 200", resp.StatusCode)
	}
	var out syncResult
	if err := jsonDecode(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Detail != "" {
		t.Fatalf("unexpected sync detail (session leg failed?): %s", out.Detail)
	}
	return out
}

func oneSession() []checkpoint.SessionEntry {
	return []checkpoint.SessionEntry{{
		SessionID: "s1", ExternalID: "ext-1", Backend: agent.BackendOpenCode,
		ContentHash: hashA, Status: agent.StatusIdle, UpdatedAt: time.Now().UTC(),
	}}
}

// TestSyncWorktree_SessionOnlyChangeImportsOnUpToDate is the regression for
// the bug where a session-only push (code unchanged ⇒ apply state up_to_date)
// never reached the sprite: the old gate ran the session leg only on a fresh
// code apply or a worktree already marked behind, so freshly-pushed sessions
// sat in object storage and never showed in clients. The digest gate must now
// import on a changed manifest and skip an unchanged one.
func TestSyncWorktree_SessionOnlyChangeImportsOnUpToDate(t *testing.T) {
	t.Parallel()
	sprite := &recordingSprite{applyState: syncStateUpToDate}
	gw, st, syncSrv := newMaterializeGateway(t, sprite.server(t))
	seedMaterializedWorktree(t, st)

	// A session-only push: a one-session manifest now lives in storage while
	// the worktree's recorded digest is still empty.
	digest := putSessionManifest(t, syncSrv, oneSession())

	// First sync: digest differs from the stored (empty) hash ⇒ import runs.
	if got := syncWorktreeOnce(t, gw); got.State != syncStateUpToDate {
		t.Fatalf("state=%q, want up_to_date", got.State)
	}
	if got := atomic.LoadInt32(&sprite.sessionCalls); got != 1 {
		t.Fatalf("session import calls=%d after a session-only change, want 1 (the gateway skipped it)", got)
	}
	wt, err := st.GetWorktreeByID(context.Background(), matWT)
	if err != nil {
		t.Fatal(err)
	}
	if wt.SessionsSyncedHash != digest {
		t.Fatalf("sessions_synced_hash=%q, want %q (digest not persisted)", wt.SessionsSyncedHash, digest)
	}

	// Second sync, nothing changed: digest now matches ⇒ no re-import. This is
	// the efficiency property the old gate was protecting and the fix keeps.
	if got := syncWorktreeOnce(t, gw); got.State != syncStateUpToDate {
		t.Fatalf("re-sync state=%q, want up_to_date", got.State)
	}
	if got := atomic.LoadInt32(&sprite.sessionCalls); got != 1 {
		t.Fatalf("session import calls=%d on an unchanged refresh, want still 1 (re-imported needlessly)", got)
	}
}

// putRawManifest writes arbitrary bytes to matCK's session-manifest key (a
// stale/corrupt manifest the gateway can't parse).
func putRawManifest(t *testing.T, syncSrv *clanksync.Server, raw []byte) {
	t.Helper()
	pres, err := syncSrv.PresignSessionPuts(context.Background(), matUser, clanksync.SessionPresignRequest{CheckpointID: matCK})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, pres.SessionManifestPutURL, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT raw manifest: status %d", resp.StatusCode)
	}
}

// TestSyncWorktree_TolerateUnreadableManifest is the regression for a stale
// pre-v2 (or corrupt) session manifest wedging a worktree as "behind" forever
// — the magical-goldstine symptom. An unparseable manifest must skip the
// session import and leave the worktree up_to_date, not stuck behind.
func TestSyncWorktree_TolerateUnreadableManifest(t *testing.T) {
	t.Parallel()
	sprite := &recordingSprite{applyState: syncStateUpToDate}
	gw, st, syncSrv := newMaterializeGateway(t, sprite.server(t))
	seedMaterializedWorktree(t, st)
	// A pre-v2 manifest (version 1) — current code rejects the version.
	putRawManifest(t, syncSrv, []byte(`{"version":1,"checkpoint_id":"ck-mat","sessions":[]}`))

	// syncWorktreeOnce fails if the result carries a session-import error
	// detail, so a passing call already proves the manifest didn't error out.
	if got := syncWorktreeOnce(t, gw); got.State != syncStateUpToDate {
		t.Fatalf("state=%q, want up_to_date", got.State)
	}
	if n := atomic.LoadInt32(&sprite.sessionCalls); n != 0 {
		t.Fatalf("session import calls=%d, want 0 (unreadable manifest → skip)", n)
	}
	// The worktree row must stay up_to_date (checkmark), not wedge to behind.
	wt, err := st.GetWorktreeByID(context.Background(), matWT)
	if err != nil {
		t.Fatal(err)
	}
	if wt.SyncState != clanksync.SyncStateUpToDate {
		t.Fatalf("sync_state=%q, want up_to_date (unreadable manifest must not wedge behind)", wt.SyncState)
	}
}

// TestSyncWorktree_FreshApplyForcesSessionImport pins that a fresh code
// materialize (apply state applied) re-imports sessions even when the digest
// already matches — the sprite's $HOME volume (host.db + ~/work) may have been
// reset, so the gateway can't trust its recorded digest in that case.
func TestSyncWorktree_FreshApplyForcesSessionImport(t *testing.T) {
	t.Parallel()
	sprite := &recordingSprite{applyState: syncStateApplied}
	gw, st, syncSrv := newMaterializeGateway(t, sprite.server(t))
	seedMaterializedWorktree(t, st)
	digest := putSessionManifest(t, syncSrv, oneSession())

	// Record the digest as already-synced: a pure up_to_date refresh would now
	// skip. The applied state must force the import regardless.
	if err := st.UpdateWorktreeMaterialization(context.Background(), matWT, clanksync.MaterializationUpdate{
		MaterializedCheckpointID: matCK, SyncState: clanksync.SyncStateUpToDate, SessionsSyncedHash: digest,
	}); err != nil {
		t.Fatal(err)
	}

	if got := syncWorktreeOnce(t, gw); got.State != syncStateApplied {
		t.Fatalf("state=%q, want applied", got.State)
	}
	if got := atomic.LoadInt32(&sprite.sessionCalls); got != 1 {
		t.Fatalf("session import calls=%d on a fresh apply, want 1 (force ignored)", got)
	}
}
