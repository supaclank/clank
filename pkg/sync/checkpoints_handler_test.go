package sync_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/blobstore"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// memSyncStore is an in-memory SyncStore for handler tests. Real
// persistence is exercised in internal/store's sqlite-backed tests.
type memSyncStore struct {
	mu          sync.Mutex
	worktrees   map[string]clanksync.Worktree
	checkpoints map[string]clanksync.Checkpoint
	headBundles map[string]clanksync.HeadBundle // key: userID + "\x00" + tipSHA
}

func newMemSyncStore() *memSyncStore {
	return &memSyncStore{
		worktrees:   make(map[string]clanksync.Worktree),
		checkpoints: make(map[string]clanksync.Checkpoint),
		headBundles: make(map[string]clanksync.HeadBundle),
	}
}

func (m *memSyncStore) GetWorktreeByID(_ context.Context, id string) (clanksync.Worktree, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.worktrees[id]
	if !ok {
		return clanksync.Worktree{}, clanksync.ErrWorktreeNotFound
	}
	return w, nil
}
func (m *memSyncStore) ListWorktreesByUser(_ context.Context, userID string) ([]clanksync.Worktree, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []clanksync.Worktree
	for _, w := range m.worktrees {
		if w.UserID == userID {
			out = append(out, w)
		}
	}
	return out, nil
}
func (m *memSyncStore) InsertWorktree(_ context.Context, w clanksync.Worktree) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.worktrees[w.ID] = w
	return nil
}
func (m *memSyncStore) DeleteWorktree(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.worktrees, id)
	return nil
}
func (m *memSyncStore) UpdateWorktreePointer(_ context.Context, id, checkpointID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.worktrees[id]
	if !ok {
		return clanksync.ErrWorktreeNotFound
	}
	w.LatestSyncedCheckpoint = checkpointID
	w.UpdatedAt = time.Now().UTC()
	m.worktrees[id] = w
	return nil
}
func (m *memSyncStore) UpdateWorktreeMaterialization(_ context.Context, id string, u clanksync.MaterializationUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.worktrees[id]
	if !ok {
		return clanksync.ErrWorktreeNotFound
	}
	w.MaterializedCheckpointID = u.MaterializedCheckpointID
	w.SyncState = u.SyncState
	w.SyncConflictLocalHead = u.ConflictLocalHead
	w.SyncConflictRemoteHead = u.ConflictRemoteHead
	w.UpdatedAt = time.Now().UTC()
	m.worktrees[id] = w
	return nil
}
func (m *memSyncStore) GetCheckpointByID(_ context.Context, id string) (clanksync.Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.checkpoints[id]
	if !ok {
		return clanksync.Checkpoint{}, clanksync.ErrCheckpointNotFound
	}
	return c, nil
}
func (m *memSyncStore) ListCheckpointsByWorktree(_ context.Context, worktreeID string, limit int) ([]clanksync.Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []clanksync.Checkpoint
	for _, c := range m.checkpoints {
		if c.WorktreeID == worktreeID {
			out = append(out, c)
		}
	}
	return out, nil
}
func (m *memSyncStore) InsertCheckpoint(_ context.Context, c clanksync.Checkpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkpoints[c.ID] = c
	return nil
}
func (m *memSyncStore) MarkCheckpointUploaded(_ context.Context, id string, when time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.checkpoints[id]
	if !ok {
		return clanksync.ErrCheckpointNotFound
	}
	c.UploadedAt = when
	m.checkpoints[id] = c
	return nil
}
func (m *memSyncStore) UpdateCheckpointSessionsDigest(_ context.Context, id, digest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.checkpoints[id]
	if !ok {
		return clanksync.ErrCheckpointNotFound
	}
	c.SessionsContentDigest = digest
	m.checkpoints[id] = c
	return nil
}
func (m *memSyncStore) DeleteCheckpointsByWorktree(_ context.Context, worktreeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.checkpoints {
		if c.WorktreeID == worktreeID {
			delete(m.checkpoints, id)
		}
	}
	return nil
}
func (m *memSyncStore) GetHeadBundle(_ context.Context, userID, tipSHA string) (clanksync.HeadBundle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hb, ok := m.headBundles[userID+"\x00"+tipSHA]
	if !ok {
		return clanksync.HeadBundle{}, clanksync.ErrHeadBundleNotFound
	}
	return hb, nil
}
func (m *memSyncStore) InsertHeadBundle(_ context.Context, hb clanksync.HeadBundle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := hb.UserID + "\x00" + hb.TipSHA
	if _, ok := m.headBundles[k]; ok {
		return nil // INSERT OR IGNORE: first stored bundle for a tip wins
	}
	m.headBundles[k] = hb
	return nil
}
func (m *memSyncStore) DeleteHeadBundlesByUser(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, hb := range m.headBundles {
		if hb.UserID == userID {
			delete(m.headBundles, k)
		}
	}
	return nil
}

// fixedPrincipalMiddleware injects a fixed Principal so every request
// resolves to the same UserID — replaces the older fixedUserAuth that
// implemented the now-removed sync.Authenticator.
func fixedPrincipalMiddleware(userID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), auth.Principal{UserID: userID})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newTestServer(t *testing.T) (*httptest.Server, *memSyncStore, *blobstore.Memory) {
	t.Helper()
	store := newMemSyncStore()
	mem := blobstore.NewMemory()
	t.Cleanup(mem.Close)

	srv, err := clanksync.NewServer(clanksync.Config{
		Store:      store,
		Storage:    mem,
		PresignTTL: time.Minute,
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	httpSrv := httptest.NewServer(fixedPrincipalMiddleware("user-A", srv.Handler()))
	t.Cleanup(httpSrv.Close)
	return httpSrv, store, mem
}

// TestCheckpointFlow_HappyPath walks the laptop's full upload sequence:
// register worktree → create checkpoint → upload bundles to presigned
// URLs → commit checkpoint → verify pointer advanced and storage has
// the blobs.
func TestCheckpointFlow_HappyPath(t *testing.T) {
	t.Parallel()
	httpSrv, store, mem := newTestServer(t)

	// 1. Register worktree.
	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "myrepo (main)",
	})
	worktreeID := wt["id"].(string)
	if worktreeID == "" {
		t.Fatalf("missing id in worktree response: %v", wt)
	}

	// 2. Create checkpoint.
	createReq := map[string]string{
		"worktree_id":        worktreeID,
		"head_commit":        "deadbeef",
		"head_ref":           "main",
		"index_tree":         "1111",
		"worktree_tree":      "2222",
		"uncommitted_commit": "3333",
	}
	create := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints", createReq)
	checkpointID := create["checkpoint_id"].(string)
	headPutURL := create["head_bundle_put_url"].(string)
	incrPutURL := create["uncommitted_put_url"].(string)
	manifestPutURL := create["manifest_put_url"].(string)
	if checkpointID == "" || headPutURL == "" || incrPutURL == "" || manifestPutURL == "" {
		t.Fatalf("bad create response: %v", create)
	}

	// 3. Upload three blobs to the presigned URLs.
	uploadTo(t, headPutURL, []byte("HEADCOMMIT-bundle"))
	uploadTo(t, incrPutURL, []byte("INCR-bundle"))
	uploadTo(t, manifestPutURL, []byte(`{"version":1}`))

	// 4. Commit.
	commitURL := httpSrv.URL + "/v1/checkpoints/" + checkpointID + "/commit"
	commit := postJSON[map[string]any](t, commitURL, map[string]string{})
	if commit["checkpoint_id"] != checkpointID {
		t.Fatalf("commit response: %v", commit)
	}

	// 5. Verify pointer + uploaded_at + storage contents.
	updatedWt, _ := store.GetWorktreeByID(context.Background(), worktreeID)
	if updatedWt.LatestSyncedCheckpoint != checkpointID {
		t.Fatalf("pointer not advanced: %q", updatedWt.LatestSyncedCheckpoint)
	}
	updatedCk, _ := store.GetCheckpointByID(context.Background(), checkpointID)
	if updatedCk.UploadedAt.IsZero() {
		t.Fatalf("UploadedAt not set after commit")
	}

	keys := mem.Keys()
	if len(keys) != 3 {
		t.Fatalf("storage should have 3 blobs, has %d: %v", len(keys), keys)
	}
}

// TestCreateCheckpoint_DedupsHeadBundle pins the L1 win: a second
// checkpoint at the SAME HEAD (only uncommitted state changed — the
// dominant idle-autopush case) is told the head bundle is already_stored,
// so the laptop uploads nothing for it. The 58 MB history is sent once.
func TestCreateCheckpoint_DedupsHeadBundle(t *testing.T) {
	t.Parallel()
	httpSrv, _, mem := newTestServer(t)

	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{"display_name": "r"})
	worktreeID := wt["id"].(string)

	// Both checkpoints share head_commit "deadbeef"; only the worktree/
	// uncommitted state differs (as when you edit without committing).
	mkReq := func(worktreeTree string) map[string]string {
		return map[string]string{
			"worktree_id":        worktreeID,
			"head_commit":        "deadbeef",
			"head_ref":           "main",
			"index_tree":         "1111",
			"worktree_tree":      worktreeTree,
			"uncommitted_commit": worktreeTree,
		}
	}

	// First push: server has no head bundle → upload_full + a PUT URL.
	c1 := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints", mkReq("2222"))
	if c1["head_bundle_action"] != "upload_full" {
		t.Fatalf("first push action = %v, want upload_full", c1["head_bundle_action"])
	}
	headURL, _ := c1["head_bundle_put_url"].(string)
	if headURL == "" {
		t.Fatal("first push must provide a head PUT URL")
	}
	uploadTo(t, headURL, []byte("HEAD-bundle"))
	uploadTo(t, c1["uncommitted_put_url"].(string), []byte("incr1"))
	uploadTo(t, c1["manifest_put_url"].(string), []byte(`{"version":1}`))
	postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints/"+c1["checkpoint_id"].(string)+"/commit", map[string]string{})

	// Second push at the SAME HEAD → already_stored, no head PUT URL.
	c2 := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints", mkReq("3333"))
	if c2["head_bundle_action"] != "already_stored" {
		t.Fatalf("second push action = %v, want already_stored", c2["head_bundle_action"])
	}
	if u, _ := c2["head_bundle_put_url"].(string); u != "" {
		t.Fatalf("second push must NOT provide a head PUT URL, got %q", u)
	}

	// Commit still succeeds reusing the shared head bundle; the laptop
	// only uploaded the second checkpoint's uncommitted + manifest.
	uploadTo(t, c2["uncommitted_put_url"].(string), []byte("incr2"))
	uploadTo(t, c2["manifest_put_url"].(string), []byte(`{"version":1}`))
	commit := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints/"+c2["checkpoint_id"].(string)+"/commit", map[string]string{})
	if commit["checkpoint_id"] != c2["checkpoint_id"] {
		t.Fatalf("second commit failed: %v", commit)
	}

	// One shared head bundle + 2×{uncommitted, manifest} = 5 objects.
	if n := len(mem.Keys()); n != 5 {
		t.Fatalf("want 5 storage objects (1 shared head + 2 checkpoints), got %d: %v", n, mem.Keys())
	}
}

// TestCreateCheckpoint_UnknownBaseFallsBackToFull pins the completeness
// guard: when base_commit references a HEAD the server has never stored,
// the client is told to upload a FULL bundle (not an incremental whose
// base would be missing), keeping every chain complete to a baseline.
func TestCreateCheckpoint_UnknownBaseFallsBackToFull(t *testing.T) {
	t.Parallel()
	httpSrv, _, _ := newTestServer(t)

	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{"display_name": "r"})
	c := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints", map[string]string{
		"worktree_id":        wt["id"].(string),
		"head_commit":        "newhead",
		"index_tree":         "1111",
		"worktree_tree":      "2222",
		"uncommitted_commit": "3333",
		"base_commit":        "neverstored", // server has no such head bundle
	})
	if c["head_bundle_action"] != "upload_full" {
		t.Fatalf("unknown base should fall back to upload_full, got %v", c["head_bundle_action"])
	}
	if u, _ := c["head_bundle_put_url"].(string); u == "" {
		t.Fatal("upload_full must provide a head PUT URL")
	}
	if b, _ := c["head_bundle_base"].(string); b != "" {
		t.Fatalf("full bundle should have no base, got %q", b)
	}
}

// TestCommitCheckpoint_RejectsIfBlobMissing guards against premature
// commit calls where the laptop forgot to upload one or more blobs.
func TestCommitCheckpoint_RejectsIfBlobMissing(t *testing.T) {
	t.Parallel()
	httpSrv, _, _ := newTestServer(t)

	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "r",
	})
	worktreeID := wt["id"].(string)

	create := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints", map[string]string{
		"worktree_id":        worktreeID,
		"head_commit":        "x",
		"index_tree":         "x",
		"worktree_tree":      "x",
		"uncommitted_commit": "x",
	})
	checkpointID := create["checkpoint_id"].(string)

	// Upload only the manifest, omit the two bundles.
	uploadTo(t, create["manifest_put_url"].(string), []byte("{}"))

	resp := mustPostExpectStatus(t, httpSrv.URL+"/v1/checkpoints/"+checkpointID+"/commit", nil, http.StatusConflict)
	if !strings.Contains(string(resp), "head bundle") {
		t.Fatalf("expected error mentioning the head bundle, got %q", resp)
	}
}

// TestCreateCheckpoint_MissingFieldsReturns400 pins the validation
// status: an empty required field must surface as 400 with a body that
// names the missing fields, not as a 500 with a wrapped service error.
func TestCreateCheckpoint_MissingFieldsReturns400(t *testing.T) {
	t.Parallel()
	httpSrv, _, _ := newTestServer(t)
	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "r",
	})
	worktreeID := wt["id"].(string)

	// head_commit omitted.
	resp := mustPostExpectStatus(t, httpSrv.URL+"/v1/checkpoints", map[string]string{
		"worktree_id":        worktreeID,
		"index_tree":         "x",
		"worktree_tree":      "x",
		"uncommitted_commit": "x",
	}, http.StatusBadRequest)
	if !strings.Contains(string(resp), "head_commit") {
		t.Fatalf("400 body should name the missing field, got %q", resp)
	}
}

// TestMultipleLaptopsSameUserShare regression-tests the removal of
// per-device ownership: any laptop of the same user can push to the
// same worktree without a 403 (last-write-wins is the new model).
func TestMultipleLaptopsSameUserShare(t *testing.T) {
	t.Parallel()
	httpSrv, _, _ := newTestServer(t)
	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "r",
	})
	worktreeID := wt["id"].(string)

	// Any laptop of user-A may push (no DeviceID disambiguation).
	create := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints", map[string]string{
		"worktree_id":        worktreeID,
		"head_commit":        "x",
		"index_tree":         "x",
		"worktree_tree":      "x",
		"uncommitted_commit": "x",
	})
	if id, _ := create["checkpoint_id"].(string); id == "" {
		t.Fatalf("expected checkpoint_id, got %v", create)
	}
}

// TestRegisterWorktree_SurvivesPostInsertGetFailure pins the
// idempotency contract: once InsertWorktree succeeds the response
// must return 201 with the inserted row, even if a re-read fails.
// Regression for a 500 path that historically drove clients to retry
// and accumulate `-2`, `-3` suffixes for the same logical worktree
// (the suffix logic itself is gone — IDs are now ULIDs).
func TestRegisterWorktree_SurvivesPostInsertGetFailure(t *testing.T) {
	t.Parallel()
	store := &getFailingStore{memSyncStore: newMemSyncStore()}
	mem := blobstore.NewMemory()
	t.Cleanup(mem.Close)

	srv, err := clanksync.NewServer(clanksync.Config{
		Store:      store,
		Storage:    mem,
		PresignTTL: time.Minute,
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	httpSrv := httptest.NewServer(fixedPrincipalMiddleware("user-A", srv.Handler()))
	t.Cleanup(httpSrv.Close)

	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "myrepo",
	})
	id, _ := wt["id"].(string)
	if !isULID(id) {
		t.Fatalf("id = %q, want a 26-char ULID", id)
	}
	if wt["display_name"] != "myrepo" {
		t.Fatalf("display_name = %v, want %q", wt["display_name"], "myrepo")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.worktrees) != 1 {
		t.Fatalf("store has %d worktrees, want 1", len(store.worktrees))
	}
}

// TestRegisterWorktree_SameDisplayNameMintsDistinctIDs confirms the
// post-slug behaviour: two registers with the same display_name
// succeed and yield two distinct opaque IDs (no `-2` suffix dance).
func TestRegisterWorktree_SameDisplayNameMintsDistinctIDs(t *testing.T) {
	t.Parallel()
	httpSrv, _, _ := newTestServer(t)

	one := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{"display_name": "myrepo"})
	two := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{"display_name": "myrepo"})

	id1, _ := one["id"].(string)
	id2, _ := two["id"].(string)
	if !isULID(id1) || !isULID(id2) {
		t.Fatalf("ids not ULID-shaped: %q, %q", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("same id minted twice: %q", id1)
	}
	if one["display_name"] != "myrepo" || two["display_name"] != "myrepo" {
		t.Fatalf("display_name not preserved: %v / %v", one, two)
	}
}

// isULID returns true for a 26-character Crockford-base32 string,
// which is the shape of an oklog ulid.MustNew(...).String() output.
func isULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'A' && r <= 'Z' && r != 'I' && r != 'L' && r != 'O' && r != 'U':
			// ok
		default:
			return false
		}
	}
	return true
}

// getFailingStore wraps memSyncStore and returns ErrWorktreeNotFound
// from GetWorktreeByID — simulates a transient lookup blip on a row
// that does exist after a successful InsertWorktree.
type getFailingStore struct {
	*memSyncStore
}

func (g *getFailingStore) GetWorktreeByID(context.Context, string) (clanksync.Worktree, error) {
	return clanksync.Worktree{}, clanksync.ErrWorktreeNotFound
}

func postJSON[T any](t *testing.T, url string, body any) T {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s returned %d: %s", url, resp.StatusCode, respBody)
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func mustPostExpectStatus(t *testing.T, url string, body any, want int) []byte {
	t.Helper()
	var buf []byte
	if body != nil {
		var err error
		buf, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s: want %d got %d (%s)", url, want, resp.StatusCode, respBody)
	}
	return respBody
}

func uploadTo(t *testing.T, url string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s returned %d: %s", url, resp.StatusCode, respBody)
	}
}
