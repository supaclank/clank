package gateway

import (
	"bytes"
	"context"
	"fmt"
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
	// matHost is the stub provisioner's host generation. seedMaterializedWorktree
	// deliberately leaves a worktree's materialized_host_id EMPTY (≠ matHost), so
	// the gateway's early-exit and haveHead stay off by default; a test opts in by
	// recording matHost as the materialized host id.
	matHost = "host-gen-1"
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
	applyCalls   int32
	sessionCalls int32
	// applyDelay, when set, makes each apply sleep — used to force overlapping
	// in-flight syncs so a fan-out test can observe real concurrency.
	applyDelay time.Duration
	inFlight   int32
	maxFlight  int32
	// lastHeadURLs records how many head-bundle URLs the most recent apply
	// received — lets a single-worktree test assert haveHead trimmed the chain.
	lastHeadURLs int32
}

func (rs *recordingSprite) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sync/apply-from-urls", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rs.applyCalls, 1)
		var body struct {
			HeadBundleURLs []string `json:"head_bundle_urls"`
		}
		_ = jsonDecode(r.Body, &body)
		atomic.StoreInt32(&rs.lastHeadURLs, int32(len(body.HeadBundleURLs)))
		cur := atomic.AddInt32(&rs.inFlight, 1)
		for {
			prev := atomic.LoadInt32(&rs.maxFlight)
			if cur <= prev || atomic.CompareAndSwapInt32(&rs.maxFlight, prev, cur) {
				break
			}
		}
		if rs.applyDelay > 0 {
			time.Sleep(rs.applyDelay)
		}
		atomic.AddInt32(&rs.inFlight, -1)
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
// the gateway test server, the underlying store (for seeding/assertions), the
// sync server (for minting presigned session-manifest URLs), and the Memory
// backend (for asserting which objects were fetched).
func newMaterializeGateway(t *testing.T, sprite *httptest.Server) (*httptest.Server, *store.Store, *clanksync.Server, *storage.Memory) {
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
	prov := &stubProvisioner{ref: provisioner.HostRef{URL: sprite.URL, HostID: matHost}}
	g, err := NewGateway(Config{Provisioner: prov, Sync: syncSrv}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(localAuth(g.Handler(), matUser))
	t.Cleanup(gw.Close)
	return gw, st, syncSrv, mem
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
	gw, st, syncSrv, _ := newMaterializeGateway(t, sprite.server(t))
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
	gw, st, syncSrv, _ := newMaterializeGateway(t, sprite.server(t))
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
	gw, st, syncSrv, _ := newMaterializeGateway(t, sprite.server(t))
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

// TestSyncWorktree_DigestMatchSkipsManifestFetch pins the v30 fast path: when
// the checkpoint's persisted session digest already matches what the sprite
// imported (the worktree's sessions_synced_hash), autosync skips the S3
// manifest fetch entirely — no object GET, no import round-trip.
func TestSyncWorktree_DigestMatchSkipsManifestFetch(t *testing.T) {
	t.Parallel()
	sprite := &recordingSprite{applyState: syncStateUpToDate}
	gw, st, syncSrv, mem := newMaterializeGateway(t, sprite.server(t))
	seedMaterializedWorktree(t, st)

	ctx := context.Background()
	// A session set lives in storage; persist its digest on the checkpoint and
	// record it as already-imported — the steady state after a prior sync.
	digest := putSessionManifest(t, syncSrv, oneSession())
	if err := st.UpdateCheckpointSessionsDigest(ctx, matCK, digest); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateWorktreeMaterialization(ctx, matWT, clanksync.MaterializationUpdate{
		MaterializedCheckpointID: matCK, SyncState: clanksync.SyncStateUpToDate, SessionsSyncedHash: digest,
	}); err != nil {
		t.Fatal(err)
	}

	manifestKey, err := storage.KeyFor(matUser, matWT, matCK, storage.BlobSessionManifest)
	if err != nil {
		t.Fatal(err)
	}
	before := mem.GetCount(manifestKey)

	if got := syncWorktreeOnce(t, gw); got.State != syncStateUpToDate {
		t.Fatalf("state=%q, want up_to_date", got.State)
	}
	if n := atomic.LoadInt32(&sprite.sessionCalls); n != 0 {
		t.Fatalf("session import calls=%d, want 0 (digest match must skip the import)", n)
	}
	if got := mem.GetCount(manifestKey); got != before {
		t.Fatalf("manifest GET count %d (was %d): the digest fast path must skip the S3 manifest fetch", got, before)
	}
}

// TestSyncWorktree_PersistedDigestMissingManifestFallsBack guards the failed-
// upload hazard: a digest persisted at presign time whose manifest never
// landed in storage must not wedge or crash autosync. The gateway only trusts
// an EQUAL digest match to skip, so a digest that differs from the imported
// hash ("" here) falls back to the authoritative fetch, which treats a missing
// manifest as a no-op.
func TestSyncWorktree_PersistedDigestMissingManifestFallsBack(t *testing.T) {
	t.Parallel()
	sprite := &recordingSprite{applyState: syncStateUpToDate}
	gw, st, _, mem := newMaterializeGateway(t, sprite.server(t))
	seedMaterializedWorktree(t, st) // sessions_synced_hash == ""
	// Presign persisted a digest, but the manifest PUT never happened.
	if err := st.UpdateCheckpointSessionsDigest(context.Background(), matCK, hashA); err != nil {
		t.Fatal(err)
	}

	manifestKey, err := storage.KeyFor(matUser, matWT, matCK, storage.BlobSessionManifest)
	if err != nil {
		t.Fatal(err)
	}

	// syncWorktreeOnce fails on a session-leg error Detail, so a clean pass
	// already proves the missing manifest didn't error out.
	if got := syncWorktreeOnce(t, gw); got.State != syncStateUpToDate {
		t.Fatalf("state=%q, want up_to_date", got.State)
	}
	if n := atomic.LoadInt32(&sprite.sessionCalls); n != 0 {
		t.Fatalf("session import calls=%d, want 0 (missing manifest → no-op)", n)
	}
	// The mismatch must have driven the authoritative path — i.e. it DID try to
	// fetch the (absent) manifest, proving we didn't wrongly trust the digest.
	if mem.GetCount(manifestKey) == 0 {
		t.Fatalf("manifest never fetched: a digest mismatch must fall back to the authoritative fetch")
	}
}

// --- early-exit / fan-out / haveHead (v31) -----------------------------

// TestSyncWorktree_NoOpSkipsSpriteAndS3 is the headline early-exit: when the
// latest pushed checkpoint is already materialized on this host generation AND
// the sprite holds the same session set, the gateway returns up_to_date without
// dialing the sprite (no apply, no session import) or reading object storage.
func TestSyncWorktree_NoOpSkipsSpriteAndS3(t *testing.T) {
	t.Parallel()
	sprite := &recordingSprite{applyState: syncStateUpToDate}
	gw, st, syncSrv, mem := newMaterializeGateway(t, sprite.server(t))
	seedMaterializedWorktree(t, st)

	ctx := context.Background()
	// Make the worktree fully current on this host generation: persist the
	// checkpoint's session digest and record it (+ matHost) as materialized.
	digest := putSessionManifest(t, syncSrv, oneSession())
	if err := st.UpdateCheckpointSessionsDigest(ctx, matCK, digest); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateWorktreeMaterialization(ctx, matWT, clanksync.MaterializationUpdate{
		MaterializedCheckpointID: matCK,
		MaterializedHostID:       matHost,
		SyncState:                clanksync.SyncStateUpToDate,
		SessionsSyncedHash:       digest,
	}); err != nil {
		t.Fatal(err)
	}

	manifestKey, err := storage.KeyFor(matUser, matWT, matCK, storage.BlobSessionManifest)
	if err != nil {
		t.Fatal(err)
	}

	if got := syncWorktreeOnce(t, gw); got.State != syncStateUpToDate {
		t.Fatalf("state=%q, want up_to_date", got.State)
	}
	if n := atomic.LoadInt32(&sprite.applyCalls); n != 0 {
		t.Fatalf("sprite apply calls=%d, want 0 (early-exit must not dial the sprite)", n)
	}
	if n := atomic.LoadInt32(&sprite.sessionCalls); n != 0 {
		t.Fatalf("session import calls=%d, want 0", n)
	}
	if n := mem.GetCount(manifestKey); n != 0 {
		t.Fatalf("manifest GET count=%d, want 0 (early-exit must not touch object storage)", n)
	}
}

// TestSyncWorktree_EarlyExitGateClausesAreLoadBearing proves each clause of the
// early-exit matters: flipping any one of {host generation, sync_state, session
// digest} off the fully-current baseline must fall through and dial the sprite.
func TestSyncWorktree_EarlyExitGateClausesAreLoadBearing(t *testing.T) {
	t.Parallel()
	// mutate diverges one gate input from the matching baseline.
	cases := []struct {
		name   string
		mutate func(*store.Store, string) clanksync.MaterializationUpdate
	}{
		{"host generation differs", func(_ *store.Store, digest string) clanksync.MaterializationUpdate {
			return clanksync.MaterializationUpdate{MaterializedCheckpointID: matCK, MaterializedHostID: "stale-host", SyncState: clanksync.SyncStateUpToDate, SessionsSyncedHash: digest}
		}},
		{"sync_state not up_to_date", func(_ *store.Store, digest string) clanksync.MaterializationUpdate {
			return clanksync.MaterializationUpdate{MaterializedCheckpointID: matCK, MaterializedHostID: matHost, SyncState: clanksync.SyncStateBehind, SessionsSyncedHash: digest}
		}},
		{"session digest differs", func(_ *store.Store, _ string) clanksync.MaterializationUpdate {
			return clanksync.MaterializationUpdate{MaterializedCheckpointID: matCK, MaterializedHostID: matHost, SyncState: clanksync.SyncStateUpToDate, SessionsSyncedHash: hashB}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sprite := &recordingSprite{applyState: syncStateUpToDate}
			gw, st, syncSrv, _ := newMaterializeGateway(t, sprite.server(t))
			seedMaterializedWorktree(t, st)
			ctx := context.Background()
			digest := putSessionManifest(t, syncSrv, oneSession())
			if err := st.UpdateCheckpointSessionsDigest(ctx, matCK, digest); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateWorktreeMaterialization(ctx, matWT, tc.mutate(st, digest)); err != nil {
				t.Fatal(err)
			}
			_ = syncWorktreeOnce(t, gw)
			if n := atomic.LoadInt32(&sprite.applyCalls); n == 0 {
				t.Fatalf("sprite apply not dialed: the early-exit wrongly fired when %s", tc.name)
			}
		})
	}
}

// TestSyncAll_ParallelStableOrdering pins that sync-all fans out concurrently
// (bounded by defaultSyncFanoutLimit) yet returns results in worktree-iteration
// order, not completion order — the index-assignment contract. Run under -race
// to catch a regression to append-on-completion.
func TestSyncAll_ParallelStableOrdering(t *testing.T) {
	t.Parallel()
	sprite := &recordingSprite{applyState: syncStateUpToDate, applyDelay: 15 * time.Millisecond}
	gw, st, _, _ := newMaterializeGateway(t, sprite.server(t))

	const n = 12
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("wt-par-%02d", i)
		ck := "ck-" + id
		head := fmt.Sprintf("%040x", i+1)
		if err := st.InsertWorktree(ctx, clanksync.Worktree{ID: id, UserID: matUser, DisplayName: id, LatestSyncedCheckpoint: ck, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertCheckpoint(ctx, clanksync.Checkpoint{ID: ck, WorktreeID: id, HeadCommit: head, IndexTree: "i", WorktreeTree: "w", UncommittedCommit: "u", CreatedAt: now, CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkCheckpointUploaded(ctx, ck, now); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertHeadBundle(ctx, clanksync.HeadBundle{UserID: matUser, TipSHA: head, BlobKey: "blob/" + head, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	// Capture the iteration order the gateway will use BEFORE the sync — the
	// sync bumps each row's updated_at, so a post-sync re-query would reorder.
	wtsBefore, err := st.ListWorktreesByUser(ctx, matUser)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(gw.URL+"/v1/worktrees/sync", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out syncAllResponse
	if err := jsonDecode(resp.Body, &out); err != nil {
		t.Fatal(err)
	}

	if len(out.Results) != n || out.Synced != n {
		t.Fatalf("results=%d synced=%d, want %d/%d", len(out.Results), out.Synced, n, n)
	}
	// Results must follow the gateway's iteration order, not whatever order the
	// delayed applies completed in.
	for i, r := range out.Results {
		if r.WorktreeID != wtsBefore[i].ID {
			t.Fatalf("result[%d]=%s but iteration order had %s at %d: results must track iteration order, not completion order", i, r.WorktreeID, wtsBefore[i].ID, i)
		}
		if r.State != syncStateUpToDate {
			t.Fatalf("worktree %s state=%q, want up_to_date", r.WorktreeID, r.State)
		}
	}
	if got := atomic.LoadInt32(&sprite.maxFlight); got < 2 {
		t.Fatalf("max in-flight applies=%d, want >1 (the loop did not run concurrently)", got)
	}
	if got := atomic.LoadInt32(&sprite.maxFlight); got > int32(defaultSyncFanoutLimit) {
		t.Fatalf("max in-flight applies=%d, want <= %d (fan-out limit not honored)", got, defaultSyncFanoutLimit)
	}
}

// TestSyncWorktree_HaveHeadTrimsChainOnHostMatch pins #5: when the sprite's
// materialized HEAD is trusted (same host generation), DownloadCheckpointURLs
// ships only the missing head-bundle delta; when it can't be trusted (host
// generation changed), the full chain is sent so an incremental bundle whose
// base the sprite may lack can't break the apply.
func TestSyncWorktree_HaveHeadTrimsChainOnHostMatch(t *testing.T) {
	t.Parallel()
	const (
		ckBase = "ck-base"
		ckTip  = "ck-tip"
		h1     = "1111111111111111111111111111111111111111"
		h2     = "2222222222222222222222222222222222222222"
	)
	// seed wires a worktree whose latest checkpoint (ckTip@h2) sits one
	// incremental head bundle above the materialized one (ckBase@h1).
	seed := func(t *testing.T, st *store.Store, materializedHost string) {
		t.Helper()
		ctx := context.Background()
		now := time.Now().UTC()
		if err := st.InsertWorktree(ctx, clanksync.Worktree{ID: matWT, UserID: matUser, DisplayName: "demo", LatestSyncedCheckpoint: ckTip, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct{ id, head string }{{ckBase, h1}, {ckTip, h2}} {
			if err := st.InsertCheckpoint(ctx, clanksync.Checkpoint{ID: c.id, WorktreeID: matWT, HeadCommit: c.head, IndexTree: "i", WorktreeTree: "w", UncommittedCommit: "u", CreatedAt: now, CreatedBy: "test"}); err != nil {
				t.Fatal(err)
			}
			if err := st.MarkCheckpointUploaded(ctx, c.id, now); err != nil {
				t.Fatal(err)
			}
		}
		// h1 full baseline, h2 incremental from h1 — the chain DownloadCheckpointURLs walks.
		if err := st.InsertHeadBundle(ctx, clanksync.HeadBundle{UserID: matUser, TipSHA: h1, BaseSHA: "", BlobKey: "blob/h1", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertHeadBundle(ctx, clanksync.HeadBundle{UserID: matUser, TipSHA: h2, BaseSHA: h1, BlobKey: "blob/h2", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateWorktreeMaterialization(ctx, matWT, clanksync.MaterializationUpdate{
			MaterializedCheckpointID: ckBase, MaterializedHostID: materializedHost, SyncState: clanksync.SyncStateUpToDate,
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("host match trims to the delta", func(t *testing.T) {
		t.Parallel()
		sprite := &recordingSprite{applyState: syncStateUpToDate}
		gw, st, _, _ := newMaterializeGateway(t, sprite.server(t))
		seed(t, st, matHost) // matches the stub provisioner's host generation
		_ = syncWorktreeOnce(t, gw)
		if got := atomic.LoadInt32(&sprite.lastHeadURLs); got != 1 {
			t.Fatalf("head URLs sent=%d, want 1 (haveHead must trim the already-held baseline)", got)
		}
	})
	t.Run("host mismatch sends the full chain", func(t *testing.T) {
		t.Parallel()
		sprite := &recordingSprite{applyState: syncStateUpToDate}
		gw, st, _, _ := newMaterializeGateway(t, sprite.server(t))
		seed(t, st, "stale-host") // a different generation — can't trust the sprite's HEAD
		_ = syncWorktreeOnce(t, gw)
		if got := atomic.LoadInt32(&sprite.lastHeadURLs); got != 2 {
			t.Fatalf("head URLs sent=%d, want 2 (full chain when the host generation can't be trusted)", got)
		}
	})
}
