package syncclient_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/auth"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/checkpoint"
	"github.com/acksell/clank/pkg/sync/storage"
	"github.com/acksell/clank/pkg/syncclient"
)

// fixedPrincipalMiddleware injects a fixed Principal so every request
// resolves to the same UserID — stand-in for real auth in tests.
func fixedPrincipalMiddleware(userID string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), auth.Principal{UserID: userID})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TestCheckpointFlow_EndToEnd builds a real git repo, pushes it via
// syncclient.PushCheckpoint to a real sync server backed by sqlite +
// in-memory storage, then downloads the bundles and applies them to a
// fresh repo to confirm the working state restores correctly.
func TestCheckpointFlow_EndToEnd(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	mem := storage.NewMemory()
	defer mem.Close()

	srv, err := clanksync.NewServer(clanksync.Config{
		Store:      st,
		Storage:    mem,
		PresignTTL: time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(fixedPrincipalMiddleware("user-A", srv.Handler()))
	defer httpSrv.Close()

	cli, err := syncclient.New(syncclient.Config{
		BaseURL:   httpSrv.URL,
		AuthToken: "test-bearer",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Set up a git repo with mixed content: committed + staged + untracked.
	repo := setupRepo(t, ctx)
	writeFile(t, repo, "main.go", "package main\nfunc main(){}\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "initial")
	writeFile(t, repo, "staged.txt", "staged content\n")
	gitMustRun(t, ctx, repo, "add", "staged.txt")
	writeFile(t, repo, "untracked.md", "# untracked\n")

	wtID, err := cli.RegisterWorktree(ctx, "myrepo (main)")
	if err != nil {
		t.Fatalf("RegisterWorktree: %v", err)
	}

	pushRes, err := cli.PushCheckpoint(ctx, wtID, repo)
	if err != nil {
		t.Fatalf("PushCheckpoint: %v", err)
	}
	if pushRes.CheckpointID == "" || pushRes.Manifest == nil {
		t.Fatalf("bad push result: %+v", pushRes)
	}

	// Verify storage layout: 3 blobs under the right key.
	keys := mem.Keys()
	if len(keys) != 3 {
		t.Fatalf("want 3 storage objects, got %d: %v", len(keys), keys)
	}
	prefix := "checkpoints/user-A/" + wtID + "/" + pushRes.CheckpointID + "/"
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			t.Fatalf("key %q missing prefix %q", k, prefix)
		}
	}

	// Verify the worktree pointer advanced and uploaded_at is set.
	wt, err := st.GetWorktreeByID(ctx, wtID)
	if err != nil {
		t.Fatal(err)
	}
	if wt.LatestSyncedCheckpoint != pushRes.CheckpointID {
		t.Fatalf("pointer not advanced: %q", wt.LatestSyncedCheckpoint)
	}
	ck, err := st.GetCheckpointByID(ctx, pushRes.CheckpointID)
	if err != nil {
		t.Fatal(err)
	}
	if ck.UploadedAt.IsZero() {
		t.Fatal("uploaded_at not set after commit")
	}

	// Pull the bundles back from storage and apply to a fresh repo.
	dest := t.TempDir()
	headBundle, _ := mem.Get(prefix + "headCommit.bundle")
	incrBundle, _ := mem.Get(prefix + "incremental.bundle")
	if len(headBundle) == 0 || len(incrBundle) == 0 {
		t.Fatalf("missing bundles in storage; keys: %v", keys)
	}

	if err := checkpoint.Apply(ctx, dest,
		pushRes.Manifest,
		bytes.NewReader(headBundle),
		bytes.NewReader(incrBundle),
	); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Verify the destination matches: HEAD SHA, file contents.
	gotHead := strings.TrimSpace(gitMustOutput(t, ctx, dest, "rev-parse", "HEAD"))
	if gotHead != pushRes.Manifest.HeadCommit {
		t.Fatalf("dest HEAD = %q, want %q", gotHead, pushRes.Manifest.HeadCommit)
	}
	for rel, wantContent := range map[string]string{
		"main.go":      "package main\nfunc main(){}\n",
		"staged.txt":   "staged content\n",
		"untracked.md": "# untracked\n",
	} {
		gotContent, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(gotContent) != wantContent {
			t.Fatalf("%s mismatch: got %q want %q", rel, gotContent, wantContent)
		}
	}
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
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}
