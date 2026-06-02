package clankcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/pkg/sync/checkpoint"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// TestApplyRemotePull exercises pull's laptop side against a real
// checkpoint built from a source repo: a fast-forwardable dest is
// updated to the sandbox state (including uncommitted changes), and a
// diverged dest is refused without being touched.
func TestApplyRemotePull(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()

	src := t.TempDir()
	pgit(t, src, "init", "-q")
	pgit(t, src, "config", "user.email", "t@e.com")
	pgit(t, src, "config", "user.name", "t")
	pgit(t, src, "config", "commit.gpgsign", "false")
	pWrite(t, filepath.Join(src, "f.txt"), "v1")
	pgit(t, src, "add", ".")
	pgit(t, src, "commit", "-qm", "c1")
	commit1 := pRev(t, src, "HEAD")
	pWrite(t, filepath.Join(src, "f.txt"), "v2")
	pgit(t, src, "add", ".")
	pgit(t, src, "commit", "-qm", "c2")
	commit2 := pRev(t, src, "HEAD")
	// Uncommitted change on the sandbox — must ride along in the checkpoint.
	pWrite(t, filepath.Join(src, "f.txt"), "v2-uncommitted")

	res, err := checkpoint.NewBuilder(src, "sprite:test").Build(ctx, "ckpt1")
	if err != nil {
		t.Fatalf("build checkpoint: %v", err)
	}
	defer res.Cleanup()

	manifestBytes, err := res.Manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest":
			_, _ = w.Write(manifestBytes)
		case "/head":
			http.ServeFile(w, r, res.HeadCommitBundle)
		case "/incr":
			http.ServeFile(w, r, res.UncommittedBundle)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	mres := &syncclient.PullResult{
		CheckpointID:   "ckpt1",
		ManifestURL:    srv.URL + "/manifest",
		HeadBundles:    []syncclient.PullHeadBundle{{TipSHA: res.Manifest.HeadCommit, GetURL: srv.URL + "/head"}},
		UncommittedURL: srv.URL + "/incr",
	}

	// Fast-forwardable dest (clone reset to commit1) → applies the sandbox state.
	dst := t.TempDir()
	pgit(t, "", "clone", "-q", src, dst)
	pgit(t, dst, "reset", "--hard", commit1)
	if err := applyRemotePull(ctx, srv.Client(), dst, mres); err != nil {
		t.Fatalf("applyRemotePull (fast-forward): %v", err)
	}
	if got := pRev(t, dst, "HEAD"); got != commit2 {
		t.Errorf("dest HEAD = %s, want %s (fast-forwarded to sandbox tip)", got, commit2)
	}
	if got := pRead(t, filepath.Join(dst, "f.txt")); got != "v2-uncommitted" {
		t.Errorf("worktree content = %q, want the sandbox's uncommitted state", got)
	}

	// Empty dest (no commits) → applies the sandbox state from scratch.
	dst3 := t.TempDir()
	pgit(t, dst3, "init", "-q")
	pgit(t, dst3, "config", "user.email", "t@e.com")
	pgit(t, dst3, "config", "user.name", "t")
	if err := applyRemotePull(ctx, srv.Client(), dst3, mres); err != nil {
		t.Fatalf("applyRemotePull (empty repo): %v", err)
	}
	if got := pRev(t, dst3, "HEAD"); got != commit2 {
		t.Errorf("empty dest HEAD = %s, want %s after pull", got, commit2)
	}
	if got := pRead(t, filepath.Join(dst3, "f.txt")); got != "v2-uncommitted" {
		t.Errorf("empty dest content = %q, want sandbox uncommitted state", got)
	}

	// Diverged dest → refused, untouched.
	dst2 := t.TempDir()
	pgit(t, "", "clone", "-q", src, dst2)
	pgit(t, dst2, "config", "commit.gpgsign", "false")
	pgit(t, dst2, "reset", "--hard", commit1)
	pWrite(t, filepath.Join(dst2, "f.txt"), "local-divergent")
	pgit(t, dst2, "add", ".")
	pgit(t, dst2, "commit", "-qm", "local")
	localTip := pRev(t, dst2, "HEAD")
	err = applyRemotePull(ctx, srv.Client(), dst2, mres)
	if err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("diverged pull should be refused with a diverged error, got %v", err)
	}
	if got := pRev(t, dst2, "HEAD"); got != localTip {
		t.Errorf("diverged dest HEAD changed to %s; pull must not touch a diverged worktree", got)
	}
}

func pgit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func pRev(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func pWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
