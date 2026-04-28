package sync

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeBundle creates a bare-source repo with one commit on `branch`,
// then runs `git bundle create` to produce a bundle of that branch's
// full history. Returns the bundle bytes and the SHA of the branch tip.
func makeBundle(t *testing.T, branch string, fileName, fileContents string) ([]byte, string) {
	t.Helper()
	src := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-b", branch, src)
	if err := os.WriteFile(filepath.Join(src, fileName), []byte(fileContents), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("-C", src, "add", fileName)
	run("-C", src, "commit", "-m", "init")

	revParse := exec.Command("git", "-C", src, "rev-parse", branch)
	tipBytes, err := revParse.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	tip := strings.TrimSpace(string(tipBytes))

	bundlePath := filepath.Join(t.TempDir(), "out.bundle")
	run("-C", src, "bundle", "create", bundlePath, branch)
	body, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	return body, tip
}

func TestMirrorRoot_UnbundleRoundTrip(t *testing.T) {
	body, tip := makeBundle(t, "feat/x", "hello.txt", "hi from feat-x\n")

	root, err := NewMirrorRoot(t.TempDir())
	if err != nil {
		t.Fatalf("NewMirrorRoot: %v", err)
	}
	mirror, err := root.Mirror("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}

	gotTip, err := mirror.Unbundle(context.Background(), "feat/x", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Unbundle: %v", err)
	}
	if gotTip != tip {
		t.Errorf("tip mismatch: want %s, got %s", tip, gotTip)
	}

	// The bare repo must now have the branch ref.
	refTip, err := mirror.RefHead(context.Background(), "refs/heads/feat/x")
	if err != nil {
		t.Fatalf("RefHead: %v", err)
	}
	if refTip != tip {
		t.Errorf("ref tip mismatch: want %s, got %s", tip, refTip)
	}

	// And the mirror must be clone-able as a real git repo. This
	// proves the mirror is the source of truth a sandbox can read.
	cloneDest := t.TempDir()
	clone := exec.Command("git", "clone", "-b", "feat/x", mirror.Path(), filepath.Join(cloneDest, "out"))
	clone.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("git clone from mirror: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(cloneDest, "out", "hello.txt"))
	if err != nil {
		t.Fatalf("read cloned file: %v", err)
	}
	if string(got) != "hi from feat-x\n" {
		t.Errorf("clone contents wrong: %q", got)
	}
}

func TestMirrorRoot_InvalidRepoKey(t *testing.T) {
	root, err := NewMirrorRoot(t.TempDir())
	if err != nil {
		t.Fatalf("NewMirrorRoot: %v", err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "foo\x00bar"} {
		if _, err := root.Mirror(bad); err == nil {
			t.Errorf("Mirror(%q) should have failed", bad)
		}
	}
}

func TestMirror_UnbundleRejectsBadBranchName(t *testing.T) {
	root, _ := NewMirrorRoot(t.TempDir())
	mirror, err := root.Mirror("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	for _, bad := range []string{"", "@", " ", "x:y", "a..b", "a^b", "/leading", "trailing/"} {
		if _, err := mirror.Unbundle(context.Background(), bad, bytes.NewReader([]byte("ignored"))); err == nil {
			t.Errorf("Unbundle with branch=%q should have failed", bad)
		}
	}
}

func TestRepoKey_Stable(t *testing.T) {
	// Different URLs → different keys; same URL → same key.
	a := RepoKey("https://github.com/foo/bar.git")
	b := RepoKey("https://github.com/foo/bar.git")
	c := RepoKey("https://github.com/foo/baz.git")
	if a != b {
		t.Errorf("RepoKey not deterministic: %s vs %s", a, b)
	}
	if a == c {
		t.Errorf("distinct URLs collided: %s", a)
	}
	if len(a) != 64 {
		t.Errorf("RepoKey should be 64 hex chars, got %d (%q)", len(a), a)
	}
	if RepoKey("") != "" {
		t.Errorf("RepoKey(\"\") should be empty")
	}
}
