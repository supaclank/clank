package clankcli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sync/checkpoint"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// pullFixture is a sandbox checkpoint served over HTTP plus the commits
// the dest repos fast-forward to. pullHits counts how many times the
// pull (materialize) endpoint was invoked, so tests can assert that
// fail-fast paths never wake the sandbox.
type pullFixture struct {
	src      string
	srv      *httptest.Server
	cli      *syncclient.Client
	commit1  string
	commit2  string
	pullHits *int32
}

func newPullFixture(t *testing.T) *pullFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()

	src := t.TempDir()
	pgit(t, src, "init", "-q")
	pgit(t, src, "config", "user.email", "t@e.com")
	pgit(t, src, "config", "user.name", "t")
	pWrite(t, filepath.Join(src, "f.txt"), "v1")
	pgit(t, src, "add", ".")
	pgit(t, src, "commit", "-qm", "c1")
	commit1 := pRev(t, src, "HEAD")
	pWrite(t, filepath.Join(src, "f.txt"), "v2")
	pgit(t, src, "add", ".")
	pgit(t, src, "commit", "-qm", "c2")
	commit2 := pRev(t, src, "HEAD")
	pWrite(t, filepath.Join(src, "f.txt"), "v2-uncommitted")

	res, err := checkpoint.NewBuilder(src, "sprite:test").Build(ctx, "ckpt1")
	if err != nil {
		t.Fatalf("build checkpoint: %v", err)
	}
	t.Cleanup(res.Cleanup)
	manifestBytes, err := res.Manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	var pullHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pull"):
			atomic.AddInt32(&pullHits, 1)
			base := "http://" + r.Host
			_, _ = w.Write([]byte(`{
				"checkpoint_id": "ckpt1",
				"manifest_url": "` + base + `/manifest",
				"head_bundles": [{"tip_sha": "ckpt1", "get_url": "` + base + `/head"}],
				"uncommitted_url": "` + base + `/incr"
			}`))
		case r.URL.Path == "/manifest":
			_, _ = w.Write(manifestBytes)
		case r.URL.Path == "/head":
			http.ServeFile(w, r, res.HeadCommitBundle)
		case r.URL.Path == "/incr":
			http.ServeFile(w, r, res.UncommittedBundle)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cli, err := syncclient.New(syncclient.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("syncclient.New: %v", err)
	}
	return &pullFixture{src: src, srv: srv, cli: cli, commit1: commit1, commit2: commit2, pullHits: &pullHits}
}

// trackedClone makes a dest repo cloned from src, reset to commit1, with
// a cached worktree id so runPull treats it as tracked.
func trackedClone(t *testing.T, f *pullFixture) string {
	t.Helper()
	dst := t.TempDir()
	pgit(t, "", "clone", "-q", f.src, dst)
	pgit(t, dst, "reset", "--hard", f.commit1)
	if err := agent.WriteLocalWorktreeID(dst, "wt-1"); err != nil {
		t.Fatalf("WriteLocalWorktreeID: %v", err)
	}
	return dst
}

func testCmd(stdin string) (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetIn(strings.NewReader(stdin))
	return cmd, out
}

func TestRunPull_CleanFastForwardApplies(t *testing.T) {
	t.Parallel()
	f := newPullFixture(t)
	dst := trackedClone(t, f)

	cmd, out := testCmd("")
	if err := runPull(context.Background(), cmd, f.cli, dst, true /* assumeYes */); err != nil {
		t.Fatalf("runPull: %v\n%s", err, out.String())
	}
	if got := pRev(t, dst, "HEAD"); got != f.commit2 {
		t.Errorf("dest HEAD = %s, want %s (fast-forwarded to sandbox tip)", got, f.commit2)
	}
	if got := pRead(t, filepath.Join(dst, "f.txt")); got != "v2-uncommitted" {
		t.Errorf("worktree content = %q, want sandbox's uncommitted state", got)
	}
	if *f.pullHits != 1 {
		t.Errorf("pull endpoint hit %d times, want 1", *f.pullHits)
	}
}

func TestRunPull_DirtyRefusedBeforeWake(t *testing.T) {
	t.Parallel()
	f := newPullFixture(t)
	dst := trackedClone(t, f)
	pWrite(t, filepath.Join(dst, "f.txt"), "local-dirty") // uncommitted

	cmd, _ := testCmd("")
	err := runPull(context.Background(), cmd, f.cli, dst, true)
	if err == nil || !strings.Contains(err.Error(), "stash") {
		t.Fatalf("dirty pull should refuse with a stash hint, got %v", err)
	}
	if *f.pullHits != 0 {
		t.Errorf("dirty pull woke the sandbox (%d hits); must fail before materialize", *f.pullHits)
	}
}

func TestRunPull_DivergedRefused(t *testing.T) {
	t.Parallel()
	f := newPullFixture(t)
	dst := trackedClone(t, f)
	// Local commit not on the sandbox → diverged (clean tree, but not ff).
	pWrite(t, filepath.Join(dst, "f.txt"), "local-divergent")
	pgit(t, dst, "add", ".")
	pgit(t, dst, "commit", "-qm", "local")
	localTip := pRev(t, dst, "HEAD")

	cmd, _ := testCmd("")
	err := runPull(context.Background(), cmd, f.cli, dst, true)
	if err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("diverged pull should refuse with a diverged error, got %v", err)
	}
	if got := pRev(t, dst, "HEAD"); got != localTip {
		t.Errorf("diverged dest HEAD changed to %s; pull must not touch it", got)
	}
}

func TestRunPull_UntrackedRefused(t *testing.T) {
	t.Parallel()
	f := newPullFixture(t)
	dst := t.TempDir()
	pgit(t, "", "clone", "-q", f.src, dst)
	pgit(t, dst, "reset", "--hard", f.commit1)
	// No WriteLocalWorktreeID → untracked.

	cmd, _ := testCmd("")
	err := runPull(context.Background(), cmd, f.cli, dst, true)
	if err == nil || !strings.Contains(err.Error(), "isn't tracked") {
		t.Fatalf("untracked pull should refuse, got %v", err)
	}
	if *f.pullHits != 0 {
		t.Errorf("untracked pull woke the sandbox (%d hits)", *f.pullHits)
	}
}

func TestRunPull_DeclineConfirmAborts(t *testing.T) {
	t.Parallel()
	f := newPullFixture(t)
	dst := trackedClone(t, f)

	cmd, out := testCmd("n\n")
	if err := runPull(context.Background(), cmd, f.cli, dst, false /* prompt */); err != nil {
		t.Fatalf("declining should not error, got %v", err)
	}
	if !strings.Contains(out.String(), "Aborted") {
		t.Errorf("expected 'Aborted' in output, got %q", out.String())
	}
	if *f.pullHits != 0 {
		t.Errorf("declined pull woke the sandbox (%d hits)", *f.pullHits)
	}
	if got := pRev(t, dst, "HEAD"); got != f.commit1 {
		t.Errorf("declined pull moved HEAD to %s; must not touch the repo", got)
	}
}
