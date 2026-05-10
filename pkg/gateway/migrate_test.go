package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/gateway"
	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/checkpoint"
	"github.com/acksell/clank/pkg/sync/storage"
	"github.com/acksell/clank/pkg/syncclient"
)

// TestMigrate_ToSprite_EndToEnd is the P3 happy-path test.
//
// Setup:
//
//   - real sqlite SyncStore + in-memory Storage
//   - real clank-sync httptest server with checkpoint endpoints
//   - real syncclient with DeviceID "dev-laptop-1"
//   - real git repo on disk with committed + staged + untracked content
//   - laptop pushes a checkpoint via syncclient
//   - stub sprite httptest server that handles POST /sync/apply
//     (multipart) by invoking checkpoint.Apply against a temp dir
//   - stub Provisioner returning the sprite URL
//   - gateway with SyncBaseURL pointed at the sync server
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

	// 1. Sqlite store + memory storage backing clank-sync.
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	mem := storage.NewMemory()
	defer mem.Close()

	// 2. clank-sync httptest server.
	syncSrv, err := clanksync.NewServer(clanksync.Config{
		Auth:        fixedUserAuth{userID: userID},
		Store:       st,
		Storage:     mem,
		PresignTTL:  2 * time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	syncHTTP := httptest.NewServer(syncSrv.Handler())
	defer syncHTTP.Close()

	// 3. Real git repo.
	repo := setupRepo(t, ctx)
	writeFile(t, repo, "main.go", "package main\nfunc main(){ /* migrated */ }\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "initial")
	writeFile(t, repo, "staged.txt", "staged content\n")
	gitMustRun(t, ctx, repo, "add", "staged.txt")
	writeFile(t, repo, "main.go", "package main\nfunc main(){ /* edited but unstaged */ }\n")
	writeFile(t, repo, "untracked.md", "# untracked\n")

	// 4. Push checkpoint via syncclient.
	cli, err := syncclient.New(syncclient.Config{
		BaseURL:  syncHTTP.URL,
		DeviceID: deviceID,
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

	// 5. Stub sprite that applies multipart checkpoints to a temp dir.
	spriteRoot := t.TempDir()
	var (
		spriteApplyMu      sync.Mutex
		spriteAppliedRepos []string
	)
	spriteHandler := http.NewServeMux()
	spriteHandler.HandleFunc("POST /sync/apply", func(w http.ResponseWriter, r *http.Request) {
		repoSlug := r.URL.Query().Get("repo")
		if repoSlug == "" {
			http.Error(w, "missing repo", http.StatusBadRequest)
			return
		}
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		manifestPart, _, err := r.FormFile("manifest")
		if err != nil {
			http.Error(w, "missing manifest: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer manifestPart.Close()
		manifestBytes, err := io.ReadAll(manifestPart)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var manifest checkpoint.Manifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			http.Error(w, "parse manifest: "+err.Error(), http.StatusBadRequest)
			return
		}
		headPart, _, err := r.FormFile("head_commit")
		if err != nil {
			http.Error(w, "missing head_commit: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer headPart.Close()
		incrPart, _, err := r.FormFile("incremental")
		if err != nil {
			http.Error(w, "missing incremental: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer incrPart.Close()

		target := filepath.Join(spriteRoot, repoSlug)
		if err := checkpoint.Apply(r.Context(), target, &manifest, headPart, incrPart); err != nil {
			http.Error(w, "apply: "+err.Error(), http.StatusInternalServerError)
			return
		}
		spriteApplyMu.Lock()
		spriteAppliedRepos = append(spriteAppliedRepos, repoSlug)
		spriteApplyMu.Unlock()

		w.WriteHeader(http.StatusNoContent)
	})
	spriteHTTP := httptest.NewServer(spriteHandler)
	defer spriteHTTP.Close()

	// 6. Stub provisioner returning the sprite URL.
	prov := &captureProvisioner{
		ref: provisioner.HostRef{
			HostID: hostID,
			URL:    spriteHTTP.URL,
		},
	}

	// 7. Gateway pointed at clank-sync + stub provisioner.
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner:   prov,
		ResolveUserID: func(*http.Request) string { return userID },
		SyncBaseURL:   syncHTTP.URL,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gwHTTP := httptest.NewServer(gw.Handler())
	defer gwHTTP.Close()

	// 8. POST migrate.
	migrateBody, _ := json.Marshal(map[string]any{
		"direction": "to_remote",
		"confirm":   true,
	})
	migrateReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		gwHTTP.URL+"/v1/migrate/worktrees/"+worktreeID,
		bytes.NewReader(migrateBody))
	migrateReq.Header.Set("Content-Type", "application/json")
	migrateReq.Header.Set("X-Clank-Device-Id", deviceID)

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

	// 9. Sync DB reflects new owner.
	wt, err := st.GetWorktreeByID(ctx, worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.OwnerKind != "remote" || wt.OwnerID != hostID {
		t.Fatalf("sync DB owner: want remote/%s, got %s/%s", hostID, wt.OwnerKind, wt.OwnerID)
	}

	// 10. EnsureHost was called.
	if prov.calls != 1 {
		t.Fatalf("provisioner.EnsureHost calls = %d, want 1", prov.calls)
	}

	// 11. Sprite-side filesystem matches the manifest.
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

// TestMigrate_RejectsWhenLaptopNotOwner ensures a request from a
// device that doesn't own the worktree fails fast (403) without
// touching the sprite.
func TestMigrate_RejectsWhenLaptopNotOwner(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const userID = "user-A"

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := storage.NewMemory()
	defer mem.Close()

	syncSrv, err := clanksync.NewServer(clanksync.Config{
		Auth:        fixedUserAuth{userID: userID},
		Store:       st,
		Storage:     mem,
		PresignTTL:  time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	syncHTTP := httptest.NewServer(syncSrv.Handler())
	defer syncHTTP.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: syncHTTP.URL, DeviceID: "owner-dev"})
	if err != nil {
		t.Fatal(err)
	}
	repo := setupRepo(t, ctx)
	writeFile(t, repo, "x.txt", "x")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "x")
	worktreeID, err := cli.RegisterWorktree(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.PushCheckpoint(ctx, worktreeID, repo); err != nil {
		t.Fatal(err)
	}

	prov := &captureProvisioner{ref: provisioner.HostRef{HostID: "h", URL: "http://unused.invalid"}}
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner:   prov,
		ResolveUserID: func(*http.Request) string { return userID },
		SyncBaseURL:   syncHTTP.URL,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gwHTTP := httptest.NewServer(gw.Handler())
	defer gwHTTP.Close()

	body, err := json.Marshal(map[string]any{"direction": "to_remote", "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gwHTTP.URL+"/v1/migrate/worktrees/"+worktreeID, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Clank-Device-Id", "imposter-dev")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 403, got %d: %s", resp.StatusCode, respBody)
	}
	if prov.calls != 0 {
		t.Fatalf("EnsureHost should not have been called on imposter; got %d calls", prov.calls)
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
		Auth:        fixedUserAuth{userID: userID},
		Store:       st,
		Storage:     mem,
		PresignTTL:  time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	syncHTTP := httptest.NewServer(syncSrv.Handler())
	defer syncHTTP.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: syncHTTP.URL, DeviceID: deviceID})
	if err != nil {
		t.Fatal(err)
	}
	worktreeID, err := cli.RegisterWorktree(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}

	prov := &captureProvisioner{ref: provisioner.HostRef{HostID: "h", URL: "http://unused.invalid"}}
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner:   prov,
		ResolveUserID: func(*http.Request) string { return userID },
		SyncBaseURL:   syncHTTP.URL,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gwHTTP := httptest.NewServer(gw.Handler())
	defer gwHTTP.Close()

	body, err := json.Marshal(map[string]any{"direction": "to_remote", "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gwHTTP.URL+"/v1/migrate/worktrees/"+worktreeID, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Clank-Device-Id", deviceID)
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
