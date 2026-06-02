package hostmux_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostmux "github.com/acksell/clank/internal/host/mux"
	"github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// applyResult mirrors the handler's 200-OK body.
type applyResultBody struct {
	State        string `json:"state"`
	LocalHead    string `json:"local_head"`
	IncomingHead string `json:"incoming_head"`
}

// --- git/checkpoint test helpers ------------------------------------

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(bytes.TrimSpace(out))
}

// commitFile writes content to file in dir and commits it, returning the
// new HEAD SHA.
func commitFile(t *testing.T, dir, file, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

// newLaptopRepo initializes a git repo with one commit and returns it.
func newLaptopRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	commitFile(t, dir, "a.txt", "one", "c1")
	return dir
}

// buildCheckpoint builds a full checkpoint of repo and serves its three
// blobs (manifest, head bundle, uncommitted) from a fresh httptest
// server, returning the manifest's HeadCommit and the three GET URLs.
func buildCheckpoint(t *testing.T, repo, ckID string) (headCommit, manifestURL, headURL, uncommittedURL string) {
	t.Helper()
	res, err := checkpoint.NewBuilder(repo, "laptop").Build(context.Background(), ckID)
	if err != nil {
		t.Fatalf("build checkpoint: %v", err)
	}
	t.Cleanup(res.Cleanup)

	manifestBytes, err := res.Manifest.Marshal()
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	headBytes, err := os.ReadFile(res.HeadCommitBundle)
	if err != nil {
		t.Fatal(err)
	}
	uncBytes, err := os.ReadFile(res.UncommittedBundle)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, _ *http.Request) { w.Write(manifestBytes) })
	mux.HandleFunc("/head", func(w http.ResponseWriter, _ *http.Request) { w.Write(headBytes) })
	mux.HandleFunc("/uncommitted", func(w http.ResponseWriter, _ *http.Request) { w.Write(uncBytes) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return res.Manifest.HeadCommit, srv.URL + "/manifest", srv.URL + "/head", srv.URL + "/uncommitted"
}

// newHostMux builds a Service + mux server backed by a temp host.db.
func newHostMux(t *testing.T) (*host.Service, *store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)
	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(srv.Close)
	return svc, st, srv
}

// apply posts an apply-from-urls request and returns the decoded result.
// It fails the test on any non-200 status (callers here only exercise
// the four success outcomes, not the error paths).
func apply(t *testing.T, srvURL, repo, manifestURL, headURL, uncommittedURL string, force bool) applyResultBody {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"repo":             repo,
		"manifest_url":     manifestURL,
		"head_bundle_urls": []string{headURL},
		"uncommitted_url":  uncommittedURL,
		"force":            force,
	})
	resp, err := http.Post(srvURL+"/sync/apply-from-urls", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 2048)
		n, _ := resp.Body.Read(buf)
		t.Fatalf("apply status %d: %s", resp.StatusCode, buf[:n])
	}
	var out applyResultBody
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func spriteRepo(home, repo string) string { return filepath.Join(home, "work", repo) }

// --- tests ----------------------------------------------------------

// TestApplyFromURLs_NewDirThenUpToDate covers fresh materialization of an
// absent worktree, then idempotent re-apply of the same checkpoint.
func TestApplyFromURLs_NewDirThenUpToDate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const repo = "wt-fresh"

	lap := newLaptopRepo(t)
	head, m, h, u := buildCheckpoint(t, lap, "ck1")
	_, _, srv := newHostMux(t)

	// Absent dir → fresh materialize.
	if got := apply(t, srv.URL, repo, m, h, u, false); got.State != "applied" {
		t.Fatalf("first apply: state=%q, want applied", got.State)
	}
	if sprHead := git(t, spriteRepo(home, repo), "rev-parse", "HEAD"); sprHead != head {
		t.Fatalf("sprite HEAD=%s, want %s", sprHead, head)
	}

	// Same checkpoint again → no-op.
	if got := apply(t, srv.URL, repo, m, h, u, false); got.State != "up_to_date" {
		t.Fatalf("re-apply: state=%q, want up_to_date", got.State)
	}
}

// TestApplyFromURLs_FastForward covers a materialized worktree that has
// fallen behind a newer laptop push being fast-forwarded.
func TestApplyFromURLs_FastForward(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const repo = "wt-ff"

	lap := newLaptopRepo(t)
	_, m1, h1, u1 := buildCheckpoint(t, lap, "ck1")
	_, _, srv := newHostMux(t)
	if got := apply(t, srv.URL, repo, m1, h1, u1, false); got.State != "applied" {
		t.Fatalf("seed apply: state=%q", got.State)
	}

	// Laptop advances on top of c1; sprite is a clean ancestor → FF.
	c2 := commitFile(t, lap, "a.txt", "two", "c2")
	_, m2, h2, u2 := buildCheckpoint(t, lap, "ck2")
	if got := apply(t, srv.URL, repo, m2, h2, u2, false); got.State != "applied" {
		t.Fatalf("ff apply: state=%q, want applied", got.State)
	}
	if sprHead := git(t, spriteRepo(home, repo), "rev-parse", "HEAD"); sprHead != c2 {
		t.Fatalf("sprite HEAD=%s, want %s", sprHead, c2)
	}
}

// TestApplyFromURLs_DivergedConflictThenForce covers the sprite having
// its own commit (a session's work) that diverged from a newer laptop
// push: the apply refuses, and Force discards the sprite's commit.
func TestApplyFromURLs_DivergedConflictThenForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const repo = "wt-div"

	lap := newLaptopRepo(t)
	_, m1, h1, u1 := buildCheckpoint(t, lap, "ck1")
	_, _, srv := newHostMux(t)
	if got := apply(t, srv.URL, repo, m1, h1, u1, false); got.State != "applied" {
		t.Fatalf("seed apply: state=%q", got.State)
	}

	// Sprite commits its own work (diverging from the laptop line).
	spr := spriteRepo(home, repo)
	sprHead := commitFile(t, spr, "a.txt", "sprite-edit", "sprite work")

	// Laptop makes a different commit off c1 and pushes.
	c2l := commitFile(t, lap, "b.txt", "laptop", "laptop work")
	_, m2, h2, u2 := buildCheckpoint(t, lap, "ck2")

	got := apply(t, srv.URL, repo, m2, h2, u2, false)
	if got.State != "conflict" {
		t.Fatalf("diverged apply: state=%q, want conflict", got.State)
	}
	if got.LocalHead != sprHead || got.IncomingHead != c2l {
		t.Fatalf("conflict heads: local=%s incoming=%s, want %s / %s", got.LocalHead, got.IncomingHead, sprHead, c2l)
	}
	// Sprite untouched by the refused apply.
	if h := git(t, spr, "rev-parse", "HEAD"); h != sprHead {
		t.Fatalf("sprite HEAD moved on conflict: %s", h)
	}

	// Force resolves by discarding the sprite's commit.
	if got := apply(t, srv.URL, repo, m2, h2, u2, true); got.State != "applied" {
		t.Fatalf("force apply: state=%q, want applied", got.State)
	}
	if h := git(t, spr, "rev-parse", "HEAD"); h != c2l {
		t.Fatalf("sprite HEAD after force=%s, want %s", h, c2l)
	}
}

// TestApplyFromURLs_DirtyConflict covers a fast-forwardable worktree with
// uncommitted local edits: applying would clobber them, so it's a conflict.
func TestApplyFromURLs_DirtyConflict(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const repo = "wt-dirty"

	lap := newLaptopRepo(t)
	_, m1, h1, u1 := buildCheckpoint(t, lap, "ck1")
	_, _, srv := newHostMux(t)
	if got := apply(t, srv.URL, repo, m1, h1, u1, false); got.State != "applied" {
		t.Fatalf("seed apply: state=%q", got.State)
	}

	// Dirty the sprite's tree (tracked file), then push a newer laptop commit.
	spr := spriteRepo(home, repo)
	if err := os.WriteFile(filepath.Join(spr, "a.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitFile(t, lap, "a.txt", "two", "c2")
	_, m2, h2, u2 := buildCheckpoint(t, lap, "ck2")

	if got := apply(t, srv.URL, repo, m2, h2, u2, false); got.State != "conflict" {
		t.Fatalf("dirty apply: state=%q, want conflict", got.State)
	}
}

// TestApplyFromURLs_SessionRunning covers the apply being blocked while a
// session is live on the worktree — even with Force.
func TestApplyFromURLs_SessionRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const repo = "wt-busy"

	lap := newLaptopRepo(t)
	_, m, h, u := buildCheckpoint(t, lap, "ck1")
	_, st, srv := newHostMux(t)

	// Seed a busy session bound to the worktree.
	now := time.Now()
	if err := st.UpsertSession(context.Background(), agent.SessionInfo{
		ID:        "sess-busy",
		Backend:   agent.BackendOpenCode,
		Status:    agent.StatusBusy,
		GitRef:    agent.GitRef{WorktreeID: repo},
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if got := apply(t, srv.URL, repo, m, h, u, false); got.State != "session_running" {
		t.Fatalf("apply with busy session: state=%q, want session_running", got.State)
	}
	if got := apply(t, srv.URL, repo, m, h, u, true); got.State != "session_running" {
		t.Fatalf("force apply with busy session: state=%q, want session_running", got.State)
	}
	// Nothing was materialized.
	if _, err := os.Stat(spriteRepo(home, repo)); !os.IsNotExist(err) {
		t.Fatalf("expected no materialized dir, stat err=%v", err)
	}
}
