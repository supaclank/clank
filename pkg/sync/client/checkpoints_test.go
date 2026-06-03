package syncclient_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/pkg/auth"
	clanksync "github.com/acksell/clank/pkg/sync"
	"github.com/acksell/clank/pkg/sync/checkpoint"
	syncclient "github.com/acksell/clank/pkg/sync/client"
	"github.com/acksell/clank/pkg/sync/storage"
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

	wtID, err := cli.RegisterWorktree(ctx, "myrepo (main)", "")
	if err != nil {
		t.Fatalf("RegisterWorktree: %v", err)
	}

	pushRes, err := cli.PushCheckpoint(ctx, wtID, repo, "", false, nil)
	if err != nil {
		t.Fatalf("PushCheckpoint: %v", err)
	}
	if pushRes.CheckpointID == "" || pushRes.Manifest == nil {
		t.Fatalf("bad push result: %+v", pushRes)
	}

	// Verify storage layout: 3 blobs — the head bundle is content-
	// addressed under <user>/heads/, the uncommitted bundle + manifest
	// live under the per-checkpoint prefix.
	keys := mem.Keys()
	if len(keys) != 3 {
		t.Fatalf("want 3 storage objects, got %d: %v", len(keys), keys)
	}
	headKey, err := storage.KeyForHead("user-A", pushRes.Manifest.HeadCommit)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "user-A/checkpoints/" + wtID + "/" + pushRes.CheckpointID + "/"
	for _, k := range keys {
		if k != headKey && !strings.HasPrefix(k, prefix) {
			t.Fatalf("key %q is neither the head bundle %q nor under %q", k, headKey, prefix)
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
	headBundle, _ := mem.Get(headKey)
	incrBundle, _ := mem.Get(prefix + "uncommitted.bundle")
	if len(headBundle) == 0 || len(incrBundle) == 0 {
		t.Fatalf("missing bundles in storage; keys: %v", keys)
	}

	if err := checkpoint.Apply(ctx, dest,
		pushRes.Manifest,
		[]io.Reader{bytes.NewReader(headBundle)},
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

// TestPushCheckpoint_UnregisteredWorktreeReturnsTypedError pins that a
// 404 from the checkpoint-create path surfaces as the typed
// ErrWorktreeNotRegistered (not an opaque string), which is the signal
// clank push keys on to self-heal a stale local worktree-id.
func TestPushCheckpoint_UnregisteredWorktreeReturnsTypedError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := storage.NewMemory()
	defer mem.Close()
	srv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(fixedPrincipalMiddleware("user-A", srv.Handler()))
	defer httpSrv.Close()
	cli, err := syncclient.New(syncclient.Config{BaseURL: httpSrv.URL, AuthToken: "t"})
	if err != nil {
		t.Fatal(err)
	}

	repo := setupRepo(t, ctx)
	writeFile(t, repo, "main.go", "package main\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "initial")

	// Never registered → the server 404s the checkpoint-create.
	_, err = cli.PushCheckpoint(ctx, "wt-does-not-exist", repo, "", false, nil)
	if !errors.Is(err, syncclient.ErrWorktreeNotRegistered) {
		t.Fatalf("want ErrWorktreeNotRegistered, got %v", err)
	}
}

// recordingObserver captures PushObserver events for assertions.
type recordingObserver struct {
	mu           sync.Mutex
	phases       []string
	sized        int64
	uploaded     int64
	lastPhase    string // phase active when UploadSized fired
	sizedAtPhase string
	sessDone     int
	sessTotal    int
}

func (o *recordingObserver) Phase(n string) {
	o.mu.Lock()
	o.phases = append(o.phases, n)
	o.lastPhase = n
	o.mu.Unlock()
}
func (o *recordingObserver) UploadSized(t int64) {
	o.mu.Lock()
	o.sized, o.sizedAtPhase = t, o.lastPhase
	o.mu.Unlock()
}
func (o *recordingObserver) UploadProgress(u int64) { o.mu.Lock(); o.uploaded = u; o.mu.Unlock() }
func (o *recordingObserver) SessionProgress(done, total int) {
	o.mu.Lock()
	o.sessDone, o.sessTotal = done, total
	o.mu.Unlock()
}

// TestPushCheckpoint_ReportsProgress pins the progress wiring: the
// observer is told the total upload size and the counting readers report
// exactly that many bytes (no miscount), and the Uploading phase fires.
func TestPushCheckpoint_ReportsProgress(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := storage.NewMemory()
	defer mem.Close()
	srv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(fixedPrincipalMiddleware("user-A", srv.Handler()))
	defer httpSrv.Close()
	cli, err := syncclient.New(syncclient.Config{BaseURL: httpSrv.URL, AuthToken: "t"})
	if err != nil {
		t.Fatal(err)
	}

	repo := setupRepo(t, ctx)
	writeFile(t, repo, "main.go", "package main\nfunc main(){}\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "initial")

	wtID, err := cli.RegisterWorktree(ctx, "r", "")
	if err != nil {
		t.Fatal(err)
	}
	obs := &recordingObserver{}
	if _, err := cli.PushCheckpoint(ctx, wtID, repo, "", false, obs); err != nil {
		t.Fatalf("PushCheckpoint: %v", err)
	}

	if obs.sized <= 0 {
		t.Fatalf("UploadSized not reported (got %d)", obs.sized)
	}
	if obs.uploaded != obs.sized {
		t.Fatalf("counting reader miscounted: uploaded %d, sized %d", obs.uploaded, obs.sized)
	}
	if !slices.Contains(obs.phases, syncclient.PhaseUploading) {
		t.Errorf("Uploading phase not reported; phases=%v", obs.phases)
	}
	// The size must be reported while the Uploading phase is active — a UI
	// that resets per-phase counters on Phase() would otherwise show "0 B".
	if obs.sizedAtPhase != syncclient.PhaseUploading {
		t.Errorf("UploadSized fired during phase %q, want %q (Phase must precede UploadSized)", obs.sizedAtPhase, syncclient.PhaseUploading)
	}
}

// TestPushCheckpoint_CleanIgnoresUncommitted pins `clank push --clean`:
// the checkpoint captures HEAD's committed tree (not the dirty working
// tree), and a restore reproduces the committed state without the
// uncommitted edit or the untracked file.
func TestPushCheckpoint_CleanIgnoresUncommitted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := storage.NewMemory()
	defer mem.Close()
	srv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(fixedPrincipalMiddleware("user-A", srv.Handler()))
	defer httpSrv.Close()
	cli, err := syncclient.New(syncclient.Config{BaseURL: httpSrv.URL, AuthToken: "t"})
	if err != nil {
		t.Fatal(err)
	}

	repo := setupRepo(t, ctx)
	writeFile(t, repo, "main.go", "committed\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "c1")
	headTree := strings.TrimSpace(gitMustOutput(t, ctx, repo, "rev-parse", "HEAD^{tree}"))

	// Dirty the tree: a tracked edit + an untracked file. --clean must
	// ignore both.
	writeFile(t, repo, "main.go", "DIRTY uncommitted\n")
	writeFile(t, repo, "scratch.txt", "untracked\n")

	wtID, err := cli.RegisterWorktree(ctx, "r", "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := cli.PushCheckpoint(ctx, wtID, repo, "", true /* committedOnly */, nil)
	if err != nil {
		t.Fatalf("clean push: %v", err)
	}
	if res.Manifest.IndexTree != headTree || res.Manifest.WorktreeTree != headTree {
		t.Fatalf("clean push should capture HEAD's tree %s, got index=%s worktree=%s",
			headTree, res.Manifest.IndexTree, res.Manifest.WorktreeTree)
	}

	// Apply → committed content restored, dirty edit + untracked file absent.
	headKey, _ := storage.KeyForHead("user-A", res.Manifest.HeadCommit)
	prefix := "user-A/checkpoints/" + wtID + "/" + res.CheckpointID + "/"
	headBundle, _ := mem.Get(headKey)
	uncommitted, _ := mem.Get(prefix + "uncommitted.bundle")
	dest := t.TempDir()
	if err := checkpoint.Apply(ctx, dest, res.Manifest,
		[]io.Reader{bytes.NewReader(headBundle)}, bytes.NewReader(uncommitted)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "main.go")); strings.TrimSpace(string(got)) != "committed" {
		t.Errorf("restored content = %q, want the committed version", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "scratch.txt")); !os.IsNotExist(err) {
		t.Error("untracked file should not be in a --clean checkpoint")
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

// TestPushPull_IncrementalChain pins L2 end-to-end: push commit A (full
// head bundle), then commit B and push with base=A (an incremental head
// bundle), then download + apply the chain into a fresh repo and confirm
// it reconstructs B. Also checks the chain links and that haveHead trims
// the download to just the missing slice.
func TestPushPull_IncrementalChain(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := storage.NewMemory()
	defer mem.Close()
	srv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(fixedPrincipalMiddleware("user-A", srv.Handler()))
	defer httpSrv.Close()
	cli, err := syncclient.New(syncclient.Config{BaseURL: httpSrv.URL, AuthToken: "t"})
	if err != nil {
		t.Fatal(err)
	}

	repo := setupRepo(t, ctx)
	writeFile(t, repo, "main.go", "package main // v1\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "c1")
	commitA := strings.TrimSpace(gitMustOutput(t, ctx, repo, "rev-parse", "HEAD"))

	wtID, err := cli.RegisterWorktree(ctx, "r", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.PushCheckpoint(ctx, wtID, repo, "", false, nil); err != nil { // full
		t.Fatalf("push A: %v", err)
	}

	writeFile(t, repo, "main.go", "package main // v2\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "c2")
	commitB := strings.TrimSpace(gitMustOutput(t, ctx, repo, "rev-parse", "HEAD"))
	rB, err := cli.PushCheckpoint(ctx, wtID, repo, commitA, false, nil) // incremental from A
	if err != nil {
		t.Fatalf("push B: %v", err)
	}

	// Chain links: A is a full baseline, B is built from A.
	hbA, err := st.GetHeadBundle(ctx, "user-A", commitA)
	if err != nil || hbA.BaseSHA != "" {
		t.Fatalf("A should be a full baseline, got %+v err=%v", hbA, err)
	}
	hbB, err := st.GetHeadBundle(ctx, "user-A", commitB)
	if err != nil || hbB.BaseSHA != commitA {
		t.Fatalf("B should be incremental from A, got %+v err=%v", hbB, err)
	}

	// Download walk: fresh applier (have_head="") gets [A, B] in order.
	dl, err := srv.DownloadCheckpointURLs(ctx, "user-A", rB.CheckpointID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(dl.HeadBundles) != 2 || dl.HeadBundles[0].TipSHA != commitA || dl.HeadBundles[1].TipSHA != commitB {
		t.Fatalf("want chain [A,B], got %+v", dl.HeadBundles)
	}
	// An applier already at A gets only [B].
	dlAtA, err := srv.DownloadCheckpointURLs(ctx, "user-A", rB.CheckpointID, commitA)
	if err != nil {
		t.Fatal(err)
	}
	if len(dlAtA.HeadBundles) != 1 || dlAtA.HeadBundles[0].TipSHA != commitB {
		t.Fatalf("want chain [B] for have_head=A, got %+v", dlAtA.HeadBundles)
	}

	// Apply the full chain into a fresh repo → reconstructs B.
	get := func(url string) []byte {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	manifest, err := checkpoint.UnmarshalManifest(get(dl.ManifestGetURL))
	if err != nil {
		t.Fatal(err)
	}
	var heads []io.Reader
	for _, hb := range dl.HeadBundles {
		heads = append(heads, bytes.NewReader(get(hb.GetURL)))
	}
	dest := t.TempDir()
	if err := checkpoint.Apply(ctx, dest, manifest, heads, bytes.NewReader(get(dl.UncommittedURL))); err != nil {
		t.Fatalf("apply chain: %v", err)
	}
	if got := strings.TrimSpace(gitMustOutput(t, ctx, dest, "rev-parse", "HEAD")); got != commitB {
		t.Errorf("dest HEAD = %s, want B %s", got, commitB)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "main.go")); strings.TrimSpace(string(got)) != "package main // v2" {
		t.Errorf("dest content = %q, want v2", got)
	}
}

// TestPushCheckpoint_SecondPushSameHEADReusesHeadBundle pins the laptop
// side of L1: a second push after an UNCOMMITTED-only change (HEAD
// unchanged) reuses the stored head bundle — the laptop uploads only the
// new uncommitted + manifest, never re-sending history. This is the
// idle-autopush latency win.
func TestPushCheckpoint_SecondPushSameHEADReusesHeadBundle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mem := storage.NewMemory()
	defer mem.Close()
	srv, err := clanksync.NewServer(clanksync.Config{Store: st, Storage: mem, PresignTTL: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(fixedPrincipalMiddleware("user-A", srv.Handler()))
	defer httpSrv.Close()
	cli, err := syncclient.New(syncclient.Config{BaseURL: httpSrv.URL, AuthToken: "t"})
	if err != nil {
		t.Fatal(err)
	}

	repo := setupRepo(t, ctx)
	writeFile(t, repo, "main.go", "package main\n")
	gitMustRun(t, ctx, repo, "add", ".")
	gitMustRun(t, ctx, repo, "commit", "-m", "initial")

	wtID, err := cli.RegisterWorktree(ctx, "r", "")
	if err != nil {
		t.Fatal(err)
	}

	r1, err := cli.PushCheckpoint(ctx, wtID, repo, "", false, nil)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	headKey, _ := storage.KeyForHead("user-A", r1.Manifest.HeadCommit)
	if b, ok := mem.Get(headKey); !ok || len(b) == 0 {
		t.Fatalf("head bundle missing after first push; keys: %v", mem.Keys())
	}
	afterFirst := len(mem.Keys())

	// Uncommitted change only — HEAD does not move.
	writeFile(t, repo, "scratch.txt", "wip\n")

	r2, err := cli.PushCheckpoint(ctx, wtID, repo, "", false, nil)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if r2.Manifest.HeadCommit != r1.Manifest.HeadCommit {
		t.Fatalf("HEAD moved unexpectedly: %s vs %s", r2.Manifest.HeadCommit, r1.Manifest.HeadCommit)
	}
	// Exactly 2 new objects (the second checkpoint's uncommitted +
	// manifest) — the head bundle was reused, not re-uploaded.
	if added := len(mem.Keys()) - afterFirst; added != 2 {
		t.Fatalf("second push added %d objects, want 2 (no head re-upload); keys: %v", added, mem.Keys())
	}
}
