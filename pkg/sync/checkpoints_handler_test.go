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

	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/storage"
)

// memSyncStore is an in-memory SyncStore for handler tests. Real
// persistence is exercised in internal/store's sqlite-backed tests.
type memSyncStore struct {
	mu          sync.Mutex
	worktrees   map[string]clanksync.Worktree
	checkpoints map[string]clanksync.Checkpoint
}

func newMemSyncStore() *memSyncStore {
	return &memSyncStore{
		worktrees:   make(map[string]clanksync.Worktree),
		checkpoints: make(map[string]clanksync.Checkpoint),
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
func (m *memSyncStore) ListWorktreesByOwner(_ context.Context, kind clanksync.OwnerKind, ownerID string) ([]clanksync.Worktree, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []clanksync.Worktree
	for _, w := range m.worktrees {
		if w.OwnerKind == kind && w.OwnerID == ownerID {
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
func (m *memSyncStore) UpdateWorktreeOwner(_ context.Context, id string, expectedKind clanksync.OwnerKind, expectedOwnerID string, newKind clanksync.OwnerKind, newOwnerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.worktrees[id]
	if !ok {
		return clanksync.ErrWorktreeNotFound
	}
	if w.OwnerKind != expectedKind || w.OwnerID != expectedOwnerID {
		return clanksync.ErrOwnerMismatch
	}
	w.OwnerKind = newKind
	w.OwnerID = newOwnerID
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

// fixedUserAuth returns the same userID for every request.
type fixedUserAuth struct{ userID string }

func (f fixedUserAuth) Verify(*http.Request) (map[string]any, error) {
	return map[string]any{"sub": f.userID}, nil
}

func newTestServer(t *testing.T) (*httptest.Server, *memSyncStore, *storage.Memory) {
	t.Helper()
	store := newMemSyncStore()
	mem := storage.NewMemory()
	t.Cleanup(mem.Close)

	srv, err := clanksync.NewServer(clanksync.Config{
		Auth:        fixedUserAuth{userID: "user-A"},
		Store:       store,
		Storage:     mem,
		PresignTTL:  time.Minute,
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	httpSrv := httptest.NewServer(srv.Handler())
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
	if wt["owner_kind"] != "local" {
		t.Fatalf("owner_kind = %v, want laptop", wt["owner_kind"])
	}

	// 2. Create checkpoint.
	createReq := map[string]string{
		"worktree_id":        worktreeID,
		"head_commit":        "deadbeef",
		"head_ref":           "main",
		"index_tree":         "1111",
		"worktree_tree":      "2222",
		"incremental_commit": "3333",
			}
	create := postJSON[map[string]any](t, httpSrv.URL+"/v1/checkpoints", createReq)
	checkpointID := create["checkpoint_id"].(string)
	headPutURL := create["head_commit_put_url"].(string)
	incrPutURL := create["incremental_put_url"].(string)
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
		"incremental_commit": "x",
			})
	checkpointID := create["checkpoint_id"].(string)

	// Upload only the manifest, omit the two bundles.
	uploadTo(t, create["manifest_put_url"].(string), []byte("{}"))

	resp := mustPostExpectStatus(t, httpSrv.URL+"/v1/checkpoints/"+checkpointID+"/commit", nil, http.StatusConflict)
	if !strings.Contains(string(resp), "headCommit.bundle") {
		t.Fatalf("expected error mentioning headCommit, got %q", resp)
	}
}

// TestCreateCheckpoint_RejectsForeignOwner ensures the ownership
// check fires when device_id differs from the worktree owner.
func TestCreateCheckpoint_RejectsForeignOwner(t *testing.T) {
	t.Parallel()
	httpSrv, _, _ := newTestServer(t)
	wt := postJSON[map[string]any](t, httpSrv.URL+"/v1/worktrees", map[string]string{
		"display_name": "r",
	})
	worktreeID := wt["id"].(string)

	resp := mustPostExpectStatusAsDevice(t, httpSrv.URL+"/v1/checkpoints", map[string]string{
		"worktree_id":        worktreeID,
		"head_commit":        "x",
		"index_tree":         "x",
		"worktree_tree":      "x",
		"incremental_commit": "x",
	}, "evil-dev", http.StatusForbidden)
	if !strings.Contains(string(resp), "owner") {
		t.Fatalf("expected ownership error, got %q", resp)
	}
}

// defaultTestDevice is the device_id every helper uses by default.
// Tests that need to simulate a foreign device use *AsDevice variants.
const defaultTestDevice = "test-dev-1"

func postJSON[T any](t *testing.T, url string, body any) T {
	return postJSONAsDevice[T](t, url, body, defaultTestDevice)
}

func postJSONAsDevice[T any](t *testing.T, url string, body any, deviceID string) T {
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
	if deviceID != "" {
		req.Header.Set("X-Clank-Device-Id", deviceID)
	}
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
	return mustPostExpectStatusAsDevice(t, url, body, defaultTestDevice, want)
}

func mustPostExpectStatusAsDevice(t *testing.T, url string, body any, deviceID string, want int) []byte {
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
	if deviceID != "" {
		req.Header.Set("X-Clank-Device-Id", deviceID)
	}
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
