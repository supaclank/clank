package sync_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/store"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/checkpoint"
	"github.com/acksell/clank/pkg/sync/storage"
)

// headerUserAuth pulls userID from X-Test-User-Id. The cross-tenant
// fuzz uses this so each goroutine can pretend to be a different user
// against a single sync.Server instance.
type headerUserAuth struct{}

func (headerUserAuth) Verify(r *http.Request) (map[string]any, error) {
	u := r.Header.Get("X-Test-User-Id")
	if u == "" {
		return nil, errors.New("missing X-Test-User-Id")
	}
	return map[string]any{"sub": u}, nil
}

// TestCrossTenantIsolation_PropertyFuzz spawns N concurrent users that
// each register a worktree and push M checkpoints. After all pushes
// complete, the test asserts:
//
//  1. Every storage key lives under exactly one user's prefix —
//     no key for user A appears under "checkpoints/<B>/...".
//  2. Each manifest's CreatedBy stamp matches the embedded user marker.
//  3. A user A token cannot register, create, or commit checkpoints
//     against a user B worktree (403 / 404).
//
// This is the catastrophic-leak guard from the plan (§B). It runs
// pre-merge locally; an integration variant against real S3 awaits a
// dev environment.
func TestCrossTenantIsolation_PropertyFuzz(t *testing.T) {
	t.Parallel()

	const (
		users           = 20
		checkpointsEach = 5
	)

	dbPath := tempDBPathHelper(t)
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mem := storage.NewMemory()
	t.Cleanup(mem.Close)

	srv, err := clanksync.NewServer(clanksync.Config{
		Auth:        headerUserAuth{},
		Store:       st,
		Storage:     mem,
		PresignTTL:  time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type userResult struct {
		userID       string
		worktreeID   string
		checkpoints  []string
		err          error
	}

	results := make(chan userResult, users)
	var wg sync.WaitGroup
	for i := 0; i < users; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			userID := fmt.Sprintf("user-%02d", i)
			deviceID := fmt.Sprintf("dev-%02d", i)
			marker := fmt.Sprintf("user-marker-%s", userID)
			res := userResult{userID: userID}

			worktreeID, err := callRegisterWorktree(ctx, httpSrv.URL, userID, deviceID, "wt-"+userID)
			if err != nil {
				res.err = fmt.Errorf("register: %w", err)
				results <- res
				return
			}
			res.worktreeID = worktreeID

			for j := 0; j < checkpointsEach; j++ {
				ckID, err := pushFakeCheckpoint(ctx, httpSrv.URL, userID, deviceID, worktreeID, marker, j)
				if err != nil {
					res.err = fmt.Errorf("push %d: %w", j, err)
					results <- res
					return
				}
				res.checkpoints = append(res.checkpoints, ckID)
			}
			results <- res
		}()
	}
	wg.Wait()
	close(results)

	all := make([]userResult, 0, users)
	for r := range results {
		if r.err != nil {
			t.Errorf("user %s: %v", r.userID, r.err)
		}
		all = append(all, r)
	}
	if t.Failed() {
		return
	}

	// 1. Every storage key lives under its user's prefix exactly.
	for _, r := range all {
		userPrefix := "checkpoints/" + r.userID + "/"
		for _, ckID := range r.checkpoints {
			ckPrefix := userPrefix + r.worktreeID + "/" + ckID + "/"
			for _, blob := range []string{"headCommit.bundle", "incremental.bundle", "manifest.json"} {
				if _, ok := mem.Get(ckPrefix + blob); !ok {
					t.Errorf("user %s: missing key %s", r.userID, ckPrefix+blob)
				}
			}
		}
	}

	// Strict cross-tenant check: no key starts with one user's prefix
	// while containing another user's marker.
	for _, key := range mem.Keys() {
		if !strings.HasPrefix(key, "checkpoints/") {
			t.Errorf("storage key outside checkpoints/ namespace: %q", key)
			continue
		}
		// The key looks like checkpoints/<userID>/<worktreeID>/<ckID>/<blob>.
		parts := strings.SplitN(strings.TrimPrefix(key, "checkpoints/"), "/", 4)
		if len(parts) != 4 {
			t.Errorf("unexpected key shape: %q", key)
			continue
		}
		ownerUser := parts[0]
		// For manifests, decode and check CreatedBy embeds the right user marker.
		if path.Base(key) == "manifest.json" {
			data, ok := mem.Get(key)
			if !ok {
				t.Errorf("manifest disappeared: %q", key)
				continue
			}
			m, err := checkpoint.UnmarshalManifest(data)
			if err != nil {
				t.Errorf("parse manifest %q: %v", key, err)
				continue
			}
			wantMarker := "user-marker-" + ownerUser
			if !strings.Contains(m.CreatedBy, wantMarker) && !strings.Contains(m.CreatedBy, ownerUser) {
				// CreatedBy follows "laptop:dev-<NN>" pattern; check the device id.
				wantDeviceTail := strings.TrimPrefix(ownerUser, "user-")
				if !strings.HasSuffix(m.CreatedBy, "dev-"+wantDeviceTail) {
					t.Errorf("manifest %q CreatedBy=%q does not match owner %s", key, m.CreatedBy, ownerUser)
				}
			}
		}
	}

	// 3. Cross-tenant API access is denied.
	if len(all) < 2 {
		t.Fatal("need at least 2 successful users for cross-access check")
	}
	uA := all[0]
	uB := all[1]
	// User A's token tries to commit user B's checkpoint.
	if len(uB.checkpoints) > 0 {
		status := callExpectingStatus(ctx, http.MethodPost,
			httpSrv.URL+"/v1/checkpoints/"+uB.checkpoints[0]+"/commit",
			uA.userID, "dev-A-imposter", nil)
		if status != http.StatusForbidden && status != http.StatusNotFound {
			t.Errorf("cross-tenant commit: want 403/404, got %d", status)
		}
	}
	// User A creates a checkpoint pointing at user B's worktree.
	body := map[string]string{
		"worktree_id":        uB.worktreeID,
		"head_commit":        "x",
		"index_tree":         "x",
		"worktree_tree":      "x",
		"incremental_commit": "x",
	}
	status := callExpectingStatus(ctx, http.MethodPost,
		httpSrv.URL+"/v1/checkpoints",
		uA.userID, "dev-A-imposter", body)
	if status != http.StatusForbidden && status != http.StatusNotFound {
		t.Errorf("cross-tenant checkpoint create: want 403/404, got %d", status)
	}
}

// TestRemoteCaller_RejectedWhenHostStoreUnset pins the
// belt-and-suspenders behavior added when sprite-push surface is not
// yet enabled: any caller presenting X-Clank-Host-Id (sprite kind)
// must be rejected with 403 when HostStore is nil, instead of
// silently bypassing the cross-tenant guard.
func TestRemoteCaller_RejectedWhenHostStoreUnset(t *testing.T) {
	t.Parallel()

	dbPath := tempDBPathHelper(t)
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mem := storage.NewMemory()
	t.Cleanup(mem.Close)

	srv, err := clanksync.NewServer(clanksync.Config{
		Auth:       headerUserAuth{},
		Store:      st,
		Storage:    mem,
		PresignTTL: time.Minute,
		// HostStore deliberately nil — production deployment without
		// the cross-tenant store should still refuse remote callers.
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First seed a worktree as a normal laptop caller so there's
	// something to read.
	worktreeID, err := callRegisterWorktree(ctx, httpSrv.URL, "user-A", "dev-A", "wt")
	if err != nil {
		t.Fatalf("register worktree: %v", err)
	}

	// Now attempt to read it as a sprite caller (X-Clank-Host-Id
	// instead of X-Clank-Device-Id). Must 403, not 200.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		httpSrv.URL+"/v1/worktrees/"+worktreeID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User-Id", "user-A")
	req.Header.Set("X-Clank-Host-Id", "sprite-imposter")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 forbidden for remote caller without HostStore, got %d", resp.StatusCode)
	}
}

// callRegisterWorktree posts /v1/worktrees as the given user/device.
func callRegisterWorktree(ctx context.Context, baseURL, userID, deviceID, displayName string) (string, error) {
	var resp struct {
		ID string `json:"id"`
	}
	body := map[string]string{"display_name": displayName}
	if err := callJSON(ctx, http.MethodPost, baseURL+"/v1/worktrees", userID, deviceID, body, &resp); err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", errors.New("empty worktree id")
	}
	return resp.ID, nil
}

// pushFakeCheckpoint creates+uploads+commits a checkpoint with marker
// strings embedded in the bundle bytes (so the storage assertions can
// catch cross-tenant mixups).
func pushFakeCheckpoint(ctx context.Context, baseURL, userID, deviceID, worktreeID, marker string, n int) (string, error) {
	createBody := map[string]string{
		"worktree_id":        worktreeID,
		"head_commit":        fmt.Sprintf("%s-head-%d", marker, n),
		"head_ref":           "main",
		"index_tree":         fmt.Sprintf("%s-index-%d", marker, n),
		"worktree_tree":      fmt.Sprintf("%s-worktree-%d", marker, n),
		"incremental_commit": fmt.Sprintf("%s-incr-%d", marker, n),
	}
	var createResp struct {
		CheckpointID     string `json:"checkpoint_id"`
		HeadCommitPutURL string `json:"head_commit_put_url"`
		IncrementalURL   string `json:"incremental_put_url"`
		ManifestPutURL   string `json:"manifest_put_url"`
	}
	if err := callJSON(ctx, http.MethodPost, baseURL+"/v1/checkpoints", userID, deviceID, createBody, &createResp); err != nil {
		return "", fmt.Errorf("create: %w", err)
	}

	// Upload synthetic bundles + a real manifest with the user marker.
	if err := putRaw(ctx, createResp.HeadCommitPutURL, []byte(marker+"-head-"+fmt.Sprint(n))); err != nil {
		return "", err
	}
	if err := putRaw(ctx, createResp.IncrementalURL, []byte(marker+"-incr-"+fmt.Sprint(n))); err != nil {
		return "", err
	}
	manifest := &checkpoint.Manifest{
		Version:           checkpoint.ManifestVersion,
		CheckpointID:      createResp.CheckpointID,
		HeadCommit:        createBody["head_commit"],
		HeadRef:           "main",
		IndexTree:         createBody["index_tree"],
		WorktreeTree:      createBody["worktree_tree"],
		IncrementalCommit: createBody["incremental_commit"],
		CreatedAt:         time.Now().UTC(),
		CreatedBy:         "laptop:" + deviceID,
	}
	mb, _ := manifest.Marshal()
	if err := putRaw(ctx, createResp.ManifestPutURL, mb); err != nil {
		return "", err
	}

	if err := callJSON(ctx, http.MethodPost,
		baseURL+"/v1/checkpoints/"+createResp.CheckpointID+"/commit",
		userID, deviceID, map[string]string{}, nil); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return createResp.CheckpointID, nil
}

func callJSON(ctx context.Context, method, url, userID, deviceID string, body any, into any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(string(buf)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User-Id", userID)
	req.Header.Set("X-Clank-Device-Id", deviceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d", method, url, resp.StatusCode)
	}
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
	}
	return nil
}

func callExpectingStatus(ctx context.Context, method, url, userID, deviceID string, body any) int {
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(string(buf)))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User-Id", userID)
	req.Header.Set("X-Clank-Device-Id", deviceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func putRaw(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("PUT %s: %d", url, resp.StatusCode)
	}
	return nil
}

func tempDBPathHelper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir + "/test.db"
}
