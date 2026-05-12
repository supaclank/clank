package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/gateway"
	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/checkpoint"
	"github.com/acksell/clank/pkg/sync/storage"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// TestMigrate_ToSprite_EndToEnd is the P3 happy-path test.
//
// Setup:
//
//   - real sqlite SyncStore + in-memory Storage
//   - real sync.Server embedded in the gateway (sync routes served via gateway)
//   - real syncclient with DeviceID "dev-laptop-1" pointing at gateway URL
//   - real git repo on disk with committed + staged + untracked content
//   - laptop pushes a checkpoint via syncclient (to the gateway, which serves sync routes)
//   - stub sprite httptest server that handles POST /sync/apply
//     (multipart) by invoking checkpoint.Apply against a temp dir
//   - stub Provisioner returning the sprite URL
//
// Action: laptop POSTs to gateway /v1/migrate/worktrees/{id} with
// {direction: to_sprite, confirm: true}, X-Clank-Device-Id header.
//
// Assertions:
//
//  1. Migration response: 200, owner_kind=sprite, owner_id=<host_id>.
//  2. Sync DB row reflects the new ownership.
//  3. Sprite-side filesystem matches the manifest exactly: HEAD SHA,
//     branch, untracked files, staged content.
//  4. Provisioner.EnsureHost was called exactly once.
func TestMigrate_ToSprite_EndToEnd(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		userID   = "user-A"
		deviceID = "dev-laptop-1"
		hostID   = "sprite-host-X"
	)

	// 1. Sqlite store + memory storage backing the embedded sync server.
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	mem := storage.NewMemory()
	defer mem.Close()

	syncSrv, err := clanksync.NewServer(clanksync.Config{
		Store:      st,
		Storage:    mem,
		PresignTTL: 2 * time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Stub sprite that handles the pull-based apply: takes presigned
	// GET URLs, fetches the bundles itself from object storage, applies
	// via checkpoint.Apply. Mirrors the real handleSyncApplyFromURLs
	// behavior without pulling in the host mux package.
	spriteRoot := t.TempDir()
	var (
		spriteApplyMu      sync.Mutex
		spriteAppliedRepos []string
	)
	spriteHandler := http.NewServeMux()
	spriteHandler.HandleFunc("POST /sync/apply-from-urls", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Repo           string `json:"repo"`
			ManifestURL    string `json:"manifest_url"`
			HeadCommitURL  string `json:"head_commit_url"`
			IncrementalURL string `json:"incremental_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Repo == "" || req.ManifestURL == "" || req.HeadCommitURL == "" || req.IncrementalURL == "" {
			http.Error(w, "missing required fields", http.StatusBadRequest)
			return
		}
		manifestBytes, err := fetchSpriteURL(r.Context(), req.ManifestURL)
		if err != nil {
			http.Error(w, "fetch manifest: "+err.Error(), http.StatusBadGateway)
			return
		}
		var manifest checkpoint.Manifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			http.Error(w, "parse manifest: "+err.Error(), http.StatusBadRequest)
			return
		}
		headBytes, err := fetchSpriteURL(r.Context(), req.HeadCommitURL)
		if err != nil {
			http.Error(w, "fetch head: "+err.Error(), http.StatusBadGateway)
			return
		}
		incrBytes, err := fetchSpriteURL(r.Context(), req.IncrementalURL)
		if err != nil {
			http.Error(w, "fetch incr: "+err.Error(), http.StatusBadGateway)
			return
		}

		target := filepath.Join(spriteRoot, req.Repo)
		if err := checkpoint.Apply(r.Context(), target, &manifest, bytes.NewReader(headBytes), bytes.NewReader(incrBytes)); err != nil {
			http.Error(w, "apply: "+err.Error(), http.StatusInternalServerError)
			return
		}
		spriteApplyMu.Lock()
		spriteAppliedRepos = append(spriteAppliedRepos, req.Repo)
		spriteApplyMu.Unlock()

		w.WriteHeader(http.StatusNoContent)
	})
	spriteHTTP := httptest.NewServer(spriteHandler)
	defer spriteHTTP.Close()

	// 3. Stub provisioner returning the sprite URL.
	prov := &captureProvisioner{
		ref: provisioner.HostRef{
			HostID: hostID,
			URL:    spriteHTTP.URL,
		},
	}

	// 4. Gateway with embedded sync server.
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: prov,
		Sync:        syncSrv,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gwHTTP := httptest.NewServer(auth.Middleware(gw.Handler(), &auth.AllowAll{UserID: userID}))
	defer gwHTTP.Close()

	// 5. Real git repo.
	repo := setupRepo(t, ctx)
	writeFile(t, repo, "main.go", "package main\nfunc main(){ /* migrated */ }\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "initial")
	writeFile(t, repo, "staged.txt", "staged content\n")
	gitMustRun(t, ctx, repo, "add", "staged.txt")
	writeFile(t, repo, "main.go", "package main\nfunc main(){ /* edited but unstaged */ }\n")
	writeFile(t, repo, "untracked.md", "# untracked\n")

	// 6. Push checkpoint via syncclient — directly to gateway (sync routes mounted there).
	cli, err := syncclient.New(syncclient.Config{
		BaseURL: gwHTTP.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	worktreeID, err := cli.RegisterWorktree(ctx, "myrepo")
	if err != nil {
		t.Fatalf("RegisterWorktree: %v", err)
	}
	pushRes, err := cli.PushCheckpoint(ctx, worktreeID, repo)
	if err != nil {
		t.Fatalf("PushCheckpoint: %v", err)
	}

	// 7. POST migrate.
	migrateBody, _ := json.Marshal(map[string]any{
		"direction": "to_remote",
		"confirm":   true,
	})
	migrateReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		gwHTTP.URL+"/v1/migrate/worktrees/"+worktreeID,
		bytes.NewReader(migrateBody))
	migrateReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(migrateReq)
	if err != nil {
		t.Fatalf("migrate request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("migrate %d: %s", resp.StatusCode, respBody)
	}

	var migrateResp struct {
		WorktreeID   string `json:"worktree_id"`
		NewOwnerKind string `json:"new_owner_kind"`
		NewOwnerID   string `json:"new_owner_id"`
		CheckpointID string `json:"checkpoint_id"`
	}
	if err := json.Unmarshal(respBody, &migrateResp); err != nil {
		t.Fatalf("decode migrate response: %v", err)
	}
	if migrateResp.NewOwnerKind != "remote" || migrateResp.NewOwnerID != hostID {
		t.Fatalf("migrate response owner: want remote/%s, got %s/%s", hostID, migrateResp.NewOwnerKind, migrateResp.NewOwnerID)
	}
	if migrateResp.CheckpointID != pushRes.CheckpointID {
		t.Fatalf("migrate checkpoint = %s, want %s", migrateResp.CheckpointID, pushRes.CheckpointID)
	}

	// 8. Sync DB reflects new owner.
	wt, err := st.GetWorktreeByID(ctx, worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.OwnerKind != "remote" || wt.OwnerID != hostID {
		t.Fatalf("sync DB owner: want remote/%s, got %s/%s", hostID, wt.OwnerKind, wt.OwnerID)
	}

	// 9. EnsureHost was called.
	if prov.calls != 1 {
		t.Fatalf("provisioner.EnsureHost calls = %d, want 1", prov.calls)
	}

	// 10. Sprite-side filesystem matches the manifest.
	spriteApplyMu.Lock()
	applied := append([]string(nil), spriteAppliedRepos...)
	spriteApplyMu.Unlock()
	if len(applied) != 1 {
		t.Fatalf("sprite applied %d times, want 1: %v", len(applied), applied)
	}
	target := filepath.Join(spriteRoot, applied[0])

	gotHead := strings.TrimSpace(gitMustOutput(t, ctx, target, "rev-parse", "HEAD"))
	if gotHead != pushRes.Manifest.HeadCommit {
		t.Fatalf("sprite HEAD = %s, want %s", gotHead, pushRes.Manifest.HeadCommit)
	}
	for rel, want := range map[string]string{
		"main.go":      "package main\nfunc main(){ /* edited but unstaged */ }\n",
		"staged.txt":   "staged content\n",
		"untracked.md": "# untracked\n",
	} {
		got, err := os.ReadFile(filepath.Join(target, rel))
		if err != nil {
			t.Fatalf("read sprite-side %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("sprite-side %s mismatch:\n got: %q\nwant: %q", rel, got, want)
		}
	}
}

// TestMigrate_TwoPhaseRoundTrip covers the migrate-back flow that
// makes `clank pull --migrate` actually move data:
//
//  1. laptop pushes a checkpoint and migrates to the sprite (P3 path)
//  2. the sprite's working tree gets edited
//  3. laptop calls /materialize: the gateway asks the sprite to
//     checkpoint, returns presigned GET URLs + a signed migration token
//  4. laptop downloads + applies the checkpoint locally (verified by
//     the fake sprite's checkpoint actually landing in S3)
//  5. laptop calls /commit: ownership atomically flips back
//
// The fake sprite handles /sync/checkpoint by using a real syncclient
// pointed at the gateway, exercising the same push flow as the laptop.
func TestMigrate_TwoPhaseRoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		userID   = "user-A"
		deviceID = "dev-laptop-1"
		hostID   = "sprite-host-X"
	)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := storage.NewMemory()
	defer mem.Close()

	syncSrv, err := clanksync.NewServer(clanksync.Config{
		Store:      st,
		Storage:    mem,
		HostStore:  st, // *store.Store implements both SyncStore and HostStore
		PresignTTL: 5 * time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Register the sprite host so the sync server's cross-check
	// (host_id → user_id) passes when the sprite uploads its checkpoint.
	if err := st.UpsertHost(ctx, hoststore.Host{
		ID:        hostID,
		UserID:    userID,
		Provider:  "test",
		Status:    hoststore.HostStatusRunning,
		AuthToken: "host-bearer",
		UpdatedAt: time.Now(),
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// The sprite's filesystem — we'll run real git operations against
	// this dir as if it were the sandbox-side working tree.
	spriteRoot := t.TempDir()
	var (
		spriteApplyMu     sync.Mutex
		spriteAppliedRepo string
	)

	spriteHandler := http.NewServeMux()
	spriteHandler.HandleFunc("POST /sync/apply-from-urls", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Repo, ManifestURL, HeadCommitURL, IncrementalURL string `json:"-"`
		}
		var body struct {
			Repo           string `json:"repo"`
			ManifestURL    string `json:"manifest_url"`
			HeadCommitURL  string `json:"head_commit_url"`
			IncrementalURL string `json:"incremental_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Repo = body.Repo
		manifestBytes, err := fetchSpriteURL(r.Context(), body.ManifestURL)
		if err != nil {
			http.Error(w, "fetch manifest: "+err.Error(), http.StatusBadGateway)
			return
		}
		var manifest checkpoint.Manifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			http.Error(w, "parse manifest: "+err.Error(), http.StatusBadRequest)
			return
		}
		headBytes, err := fetchSpriteURL(r.Context(), body.HeadCommitURL)
		if err != nil {
			http.Error(w, "fetch head: "+err.Error(), http.StatusBadGateway)
			return
		}
		incrBytes, err := fetchSpriteURL(r.Context(), body.IncrementalURL)
		if err != nil {
			http.Error(w, "fetch incr: "+err.Error(), http.StatusBadGateway)
			return
		}
		target := filepath.Join(spriteRoot, body.Repo)
		if err := checkpoint.Apply(r.Context(), target, &manifest, bytes.NewReader(headBytes), bytes.NewReader(incrBytes)); err != nil {
			http.Error(w, "apply: "+err.Error(), http.StatusInternalServerError)
			return
		}
		spriteApplyMu.Lock()
		spriteAppliedRepo = body.Repo
		spriteApplyMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	// Sprite-side pull-back endpoints, mocked. The production sprite
	// (internal/host/mux/sync.go) has /sync/build, /sync/builds/{id}/upload,
	// and DELETE /sync/builds/{id}. Inlined here to avoid pulling the
	// host-mux package into a gateway test.
	type spriteBuild struct {
		result *checkpoint.Result
		repo   string
	}
	var (
		spriteBuildsMu sync.Mutex
		spriteBuilds   = map[string]*spriteBuild{}
	)
	spriteHandler.HandleFunc("POST /sync/build", func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Query().Get("repo")
		if repo == "" {
			http.Error(w, "repo required", http.StatusBadRequest)
			return
		}
		repoPath := filepath.Join(spriteRoot, repo)
		builder := checkpoint.NewBuilder(repoPath, "sprite")
		buildID := newTestULID()
		res, err := builder.Build(r.Context(), buildID)
		if err != nil {
			http.Error(w, "build: "+err.Error(), http.StatusInternalServerError)
			return
		}
		spriteBuildsMu.Lock()
		spriteBuilds[buildID] = &spriteBuild{result: res, repo: repo}
		spriteBuildsMu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{
			"build_id":           buildID,
			"head_commit":        res.Manifest.HeadCommit,
			"head_ref":           res.Manifest.HeadRef,
			"index_tree":         res.Manifest.IndexTree,
			"worktree_tree":      res.Manifest.WorktreeTree,
			"incremental_commit": res.Manifest.IncrementalCommit,
		})
	})
	spriteHandler.HandleFunc("POST /sync/builds/{id}/upload", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			CheckpointID      string `json:"checkpoint_id"`
			ManifestPutURL    string `json:"manifest_put_url"`
			HeadCommitPutURL  string `json:"head_commit_put_url"`
			IncrementalPutURL string `json:"incremental_put_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		spriteBuildsMu.Lock()
		b := spriteBuilds[id]
		spriteBuildsMu.Unlock()
		if b == nil {
			http.Error(w, "build not found", http.StatusNotFound)
			return
		}
		b.result.Manifest.CheckpointID = body.CheckpointID
		manifestBytes, err := b.result.Manifest.Marshal()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := putFile(r.Context(), body.HeadCommitPutURL, b.result.HeadCommitBundle); err != nil {
			http.Error(w, "head: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := putFile(r.Context(), body.IncrementalPutURL, b.result.IncrementalBundle); err != nil {
			http.Error(w, "incr: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := putBytes(r.Context(), body.ManifestPutURL, manifestBytes); err != nil {
			http.Error(w, "manifest: "+err.Error(), http.StatusBadGateway)
			return
		}
		spriteBuildsMu.Lock()
		delete(spriteBuilds, id)
		spriteBuildsMu.Unlock()
		b.result.Cleanup()
		w.WriteHeader(http.StatusNoContent)
	})
	spriteHandler.HandleFunc("DELETE /sync/builds/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		spriteBuildsMu.Lock()
		b := spriteBuilds[id]
		delete(spriteBuilds, id)
		spriteBuildsMu.Unlock()
		if b != nil {
			b.result.Cleanup()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	spriteHTTP := httptest.NewServer(spriteHandler)
	defer spriteHTTP.Close()

	prov := &captureProvisioner{ref: provisioner.HostRef{HostID: hostID, URL: spriteHTTP.URL}}
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: prov,
		Sync:        syncSrv,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gwHTTP := httptest.NewServer(auth.Middleware(gw.Handler(), &auth.AllowAll{UserID: userID}))
	defer gwHTTP.Close()

	// 1. Laptop pushes a checkpoint.
	cli, err := syncclient.New(syncclient.Config{BaseURL: gwHTTP.URL})
	if err != nil {
		t.Fatal(err)
	}
	repo := setupRepo(t, ctx)
	writeFile(t, repo, "file.txt", "v1\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "v1")
	worktreeID, err := cli.RegisterWorktree(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.PushCheckpoint(ctx, worktreeID, repo); err != nil {
		t.Fatal(err)
	}

	// 2. Migrate to sprite — exercises /v1/migrate (existing path).
	migrateBody, _ := json.Marshal(map[string]any{"direction": "to_remote", "confirm": true})
	migReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		gwHTTP.URL+"/v1/migrate/worktrees/"+worktreeID, bytes.NewReader(migrateBody))
	migResp, err := http.DefaultClient.Do(migReq)
	if err != nil {
		t.Fatal(err)
	}
	if migResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(migResp.Body)
		migResp.Body.Close()
		t.Fatalf("migrate to_remote: %d: %s", migResp.StatusCode, b)
	}
	migResp.Body.Close()

	// 3. Sprite-side edit: simulate user activity in the sandbox.
	spriteApplyMu.Lock()
	repoOnSprite := filepath.Join(spriteRoot, spriteAppliedRepo)
	spriteApplyMu.Unlock()
	writeFile(t, repoOnSprite, "file.txt", "v2-from-sandbox\n")
	gitMustRun(t, ctx, repoOnSprite, "config", "user.email", "sprite@clank.local")
	gitMustRun(t, ctx, repoOnSprite, "config", "user.name", "clank-sprite")
	gitMustRun(t, ctx, repoOnSprite, "config", "commit.gpgsign", "false")
	gitMustRun(t, ctx, repoOnSprite, "add", ".")
	gitMustRun(t, ctx, repoOnSprite, "commit", "-m", "v2 on sandbox")
	expectedSpriteHead := strings.TrimSpace(gitMustOutput(t, ctx, repoOnSprite, "rev-parse", "HEAD"))

	// 4. Materialize from laptop POV.
	matReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		gwHTTP.URL+"/v1/migrate/worktrees/"+worktreeID+"/materialize",
		bytes.NewReader([]byte(`{"confirm":true}`)))
	matResp, err := http.DefaultClient.Do(matReq)
	if err != nil {
		t.Fatal(err)
	}
	defer matResp.Body.Close()
	matBody, _ := io.ReadAll(matResp.Body)
	if matResp.StatusCode != http.StatusOK {
		t.Fatalf("materialize: %d: %s", matResp.StatusCode, matBody)
	}
	var mres struct {
		CheckpointID    string `json:"checkpoint_id"`
		HeadCommit      string `json:"head_commit"`
		ManifestURL     string `json:"manifest_url"`
		HeadCommitURL   string `json:"head_commit_url"`
		IncrementalURL  string `json:"incremental_url"`
		MigrationToken  string `json:"migration_token"`
		MigrationExpiry int64  `json:"migration_expiry"`
	}
	if err := json.Unmarshal(matBody, &mres); err != nil {
		t.Fatalf("decode materialize: %v", err)
	}
	if mres.HeadCommit != expectedSpriteHead {
		t.Fatalf("materialize HEAD = %s, want sprite HEAD %s", mres.HeadCommit, expectedSpriteHead)
	}

	// 5. Download + apply locally. Use a fresh local dir to make the
	// effect visible: applying onto a destination that already matches
	// would prove little.
	localDest := t.TempDir()
	if err := pullAndApply(ctx, localDest, mres.ManifestURL, mres.HeadCommitURL, mres.IncrementalURL); err != nil {
		t.Fatalf("apply checkpoint locally: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(localDest, "file.txt"))
	if err != nil {
		t.Fatalf("read applied file: %v", err)
	}
	if string(got) != "v2-from-sandbox\n" {
		t.Fatalf("local file content = %q, want v2 from sandbox", got)
	}

	// 6. Commit ownership transfer.
	commitBody, _ := json.Marshal(map[string]any{
		"checkpoint_id":   mres.CheckpointID,
		"migration_token": mres.MigrationToken,
	})
	commitReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		gwHTTP.URL+"/v1/migrate/worktrees/"+worktreeID+"/commit", bytes.NewReader(commitBody))
	commitResp, err := http.DefaultClient.Do(commitReq)
	if err != nil {
		t.Fatal(err)
	}
	defer commitResp.Body.Close()
	if commitResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(commitResp.Body)
		t.Fatalf("commit: %d: %s", commitResp.StatusCode, b)
	}

	wt, err := st.GetWorktreeByID(ctx, worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	// Per-user local ownership: OwnerKind == local, OwnerID empty.
	if wt.OwnerKind != "local" || wt.OwnerID != "" {
		t.Fatalf("after commit: want local/(empty), got %s/%s", wt.OwnerKind, wt.OwnerID)
	}

	// 7. Commit with the same token is rejected (must not double-flip).
	commitReq2, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		gwHTTP.URL+"/v1/migrate/worktrees/"+worktreeID+"/commit", bytes.NewReader(commitBody))
	commitResp2, err := http.DefaultClient.Do(commitReq2)
	if err != nil {
		t.Fatal(err)
	}
	defer commitResp2.Body.Close()
	if commitResp2.StatusCode == http.StatusOK {
		t.Fatalf("second commit should fail (worktree no longer sprite-owned)")
	}
}

func pullAndApply(ctx context.Context, dest, manifestURL, headURL, incrURL string) error {
	cli := &http.Client{Timeout: 30 * time.Second}
	manifestBytes, err := fetchSpriteURL(ctx, manifestURL)
	if err != nil {
		return err
	}
	var manifest checkpoint.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return err
	}
	headBytes, err := fetchSpriteURL(ctx, headURL)
	if err != nil {
		return err
	}
	incrBytes, err := fetchSpriteURL(ctx, incrURL)
	if err != nil {
		return err
	}
	_ = cli
	return checkpoint.Apply(ctx, dest, &manifest, bytes.NewReader(headBytes), bytes.NewReader(incrBytes))
}

// TestMigrate_RetriesOnURLExpired locks in the gateway's one-shot
// retry for sprite-reported url_expired: the gateway mints fresh
// presigned URLs and re-POSTs to the sprite exactly once. Two
// consecutive url_expired responses propagate as an error.
func TestMigrate_RetriesOnURLExpired(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const (
		userID   = "user-A"
		deviceID = "dev-laptop-1"
		hostID   = "sprite-host-X"
	)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := storage.NewMemory()
	defer mem.Close()

	syncSrv, err := clanksync.NewServer(clanksync.Config{
		Store:      st,
		Storage:    mem,
		PresignTTL: 2 * time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Sprite that returns url_expired on the first call and succeeds
	// on the second. The retry logic should hand it fresh URLs.
	var spriteCalls int
	var spriteMu sync.Mutex
	spriteHandler := http.NewServeMux()
	spriteHandler.HandleFunc("POST /sync/apply-from-urls", func(w http.ResponseWriter, r *http.Request) {
		spriteMu.Lock()
		spriteCalls++
		thisCall := spriteCalls
		spriteMu.Unlock()
		if thisCall == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"code":"url_expired","error":"simulated expiry"}`))
			return
		}
		// Drain body to satisfy real-client expectations.
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	spriteHTTP := httptest.NewServer(spriteHandler)
	defer spriteHTTP.Close()

	prov := &captureProvisioner{
		ref: provisioner.HostRef{HostID: hostID, URL: spriteHTTP.URL},
	}
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: prov,
		Sync:        syncSrv,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gwHTTP := httptest.NewServer(auth.Middleware(gw.Handler(), &auth.AllowAll{UserID: userID}))
	defer gwHTTP.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: gwHTTP.URL})
	if err != nil {
		t.Fatal(err)
	}
	repo := setupRepo(t, ctx)
	writeFile(t, repo, "f.txt", "hello\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "init")
	worktreeID, err := cli.RegisterWorktree(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.PushCheckpoint(ctx, worktreeID, repo); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"direction": "to_remote", "confirm": true})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		gwHTTP.URL+"/v1/migrate/worktrees/"+worktreeID, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("migrate: want 200, got %d: %s", resp.StatusCode, respBody)
	}
	spriteMu.Lock()
	calls := spriteCalls
	spriteMu.Unlock()
	if calls != 2 {
		t.Fatalf("sprite calls = %d, want 2 (one url_expired + one success)", calls)
	}
}

// TestMigrate_RejectsWhenNoCheckpoint guards the pre-check: if the
// laptop has never pushed a checkpoint, the migration aborts with 409.
func TestMigrate_RejectsWhenNoCheckpoint(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const (
		userID   = "user-A"
		deviceID = "dev-1"
	)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := storage.NewMemory()
	defer mem.Close()

	syncSrv, err := clanksync.NewServer(clanksync.Config{
		Store:      st,
		Storage:    mem,
		PresignTTL: time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	prov := &captureProvisioner{ref: provisioner.HostRef{HostID: "h", URL: "http://unused.invalid"}}
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: prov,
		Sync:        syncSrv,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gwHTTP := httptest.NewServer(auth.Middleware(gw.Handler(), &auth.AllowAll{UserID: userID}))
	defer gwHTTP.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: gwHTTP.URL})
	if err != nil {
		t.Fatal(err)
	}
	worktreeID, err := cli.RegisterWorktree(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(map[string]any{"direction": "to_remote", "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gwHTTP.URL+"/v1/migrate/worktrees/"+worktreeID, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 409 unsynced, got %d: %s", resp.StatusCode, respBody)
	}
}

// fetchSpriteURL is a minimal stand-in for the real sprite's URL
// fetcher: GET the URL, read the body, surface non-2xx as an error.
func fetchSpriteURL(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return nil, errors.New("status " + http.StatusText(resp.StatusCode) + ": " + strings.TrimSpace(string(preview)))
	}
	return io.ReadAll(resp.Body)
}

// putFile streams a local file to a presigned PUT URL — the fake
// sprite's upload step. Same semantic as the production sprite's
// uploadFile in internal/host/mux/sync.go, just terser.
func putFile(ctx context.Context, putURL, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, f)
	if err != nil {
		return err
	}
	req.ContentLength = stat.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("PUT %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	return nil
}

func putBytes(ctx context.Context, putURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("PUT %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	return nil
}

func newTestULID() string {
	return ulid.Make().String()
}

// --- helpers ---

type captureProvisioner struct {
	ref   provisioner.HostRef
	calls int
}

func (p *captureProvisioner) EnsureHost(context.Context, string) (provisioner.HostRef, error) {
	p.calls++
	return p.ref, nil
}
func (*captureProvisioner) SuspendHost(context.Context, string) error { return nil }
func (*captureProvisioner) DestroyHost(context.Context, string) error { return nil }

type fixedUserAuth struct{ userID string }

func (f fixedUserAuth) Verify(*http.Request) (map[string]any, error) {
	return map[string]any{"sub": f.userID}, nil
}

func setupRepo(t *testing.T, ctx context.Context) string {
	t.Helper()
	dir := t.TempDir()
	gitMustRun(t, ctx, dir, "init", "--initial-branch=main", "--quiet")
	gitMustRun(t, ctx, dir, "config", "user.email", "test@clank.local")
	gitMustRun(t, ctx, dir, "config", "user.name", "clank-test")
	gitMustRun(t, ctx, dir, "config", "commit.gpgsign", "false")
	return dir
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitMustRun(t *testing.T, ctx context.Context, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
}

func gitMustOutput(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("git %s: %s: %v", strings.Join(args, " "), ee.Stderr, err)
		}
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}
