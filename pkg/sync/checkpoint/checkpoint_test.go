package checkpoint_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	mathrand "math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// TestRoundTrip_HappyPath builds a checkpoint of a small repo with
// committed + staged + unstaged + untracked content, wipes the repo,
// applies the checkpoint, and asserts byte-for-byte equivalence on
// every file plus exact HEAD/branch/index state.
func TestRoundTrip_HappyPath(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repo := setupRepo(t, ctx)

	// Committed file.
	writeFile(t, repo, "main.go", "package main\n\nfunc main(){}\n")
	gitMustRun(t, ctx, repo, "add", "main.go")
	gitMustRun(t, ctx, repo, "commit", "-m", "initial")

	// Staged-but-not-committed file.
	writeFile(t, repo, "staged.txt", "this is staged\n")
	gitMustRun(t, ctx, repo, "add", "staged.txt")

	// Unstaged modification to a tracked file.
	writeFile(t, repo, "main.go", "package main\n\n// edited\nfunc main(){}\n")

	// Untracked file (not in .gitignore, so it gets included via add -A).
	writeFile(t, repo, "untracked.md", "# Notes\n\nuntracked content\n")

	roundTripAndAssert(t, ctx, repo, "ck-roundtrip-happy")
}

// TestRoundTrip_DetachedHead exercises the codepath where headRef is
// empty (HEAD is detached at a SHA, not on a branch).
func TestRoundTrip_DetachedHead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repo := setupRepo(t, ctx)
	writeFile(t, repo, "a.txt", "a\n")
	gitMustRun(t, ctx, repo, "add", "a.txt")
	gitMustRun(t, ctx, repo, "commit", "-m", "first")
	writeFile(t, repo, "b.txt", "b\n")
	gitMustRun(t, ctx, repo, "add", "b.txt")
	gitMustRun(t, ctx, repo, "commit", "-m", "second")

	// Detach HEAD at HEAD~1.
	gitMustRun(t, ctx, repo, "checkout", "--detach", "HEAD~1")

	writeFile(t, repo, "uncommitted.txt", "while detached\n")

	roundTripAndAssert(t, ctx, repo, "ck-detached")
}

// TestRoundTrip_RandomTrees fuzzes with a few random working-tree
// shapes to catch edge cases the hand-written tests miss.
func TestRoundTrip_RandomTrees(t *testing.T) {
	t.Parallel()
	rng := mathrand.New(mathrand.NewPCG(42, 0xc0ffee))
	for i := 0; i < 5; i++ {
		i := i
		// Pre-roll the size before launching subtests so each subtest
		// gets a deterministic but distinct size — touching rng inside
		// the parallel subtest would race.
		size := 8 + rng.IntN(8)
		seed1, seed2 := uint64(0xCAFE+i), uint64(0xBEEF+i)
		t.Run(fmt.Sprintf("seed-%d", i), func(t *testing.T) {
			t.Parallel()
			subCtx, subCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer subCancel()
			subRng := mathrand.New(mathrand.NewPCG(seed1, seed2))
			repo := setupRepo(t, subCtx)
			seedRandomTree(t, subCtx, repo, subRng, size)
			roundTripAndAssert(t, subCtx, repo, fmt.Sprintf("ck-random-%d", i))
		})
	}
}

// TestApply_RejectsUnknownVersion guards the manifest version check.
func TestApply_RejectsUnknownVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	bad := &checkpoint.Manifest{Version: 9999}
	err := checkpoint.Apply(ctx, tmp, bad, strings.NewReader(""), strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "unsupported manifest version") {
		t.Fatalf("expected version-mismatch error, got %v", err)
	}
}

// TestApply_RemovesStaleUntracked pins the contract that re-applying
// a checkpoint produces *exactly* the manifest's worktreeTree —
// untracked files left over from a previous session must not survive.
func TestApply_RemovesStaleUntracked(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repo := setupRepo(t, ctx)
	writeFile(t, repo, "tracked.txt", "tracked\n")
	gitMustRun(t, ctx, repo, "add", "tracked.txt")
	gitMustRun(t, ctx, repo, "commit", "-m", "init")

	builder := checkpoint.NewBuilder(repo, "test:laptop")
	res, err := builder.Build(ctx, "ck-stale-clean")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()

	dest := t.TempDir()

	openBundles := func() (*os.File, *os.File) {
		head, err := os.Open(res.HeadCommitBundle)
		if err != nil {
			t.Fatal(err)
		}
		incr, err := os.Open(res.IncrementalBundle)
		if err != nil {
			head.Close()
			t.Fatal(err)
		}
		return head, incr
	}

	head1, incr1 := openBundles()
	if err := checkpoint.Apply(ctx, dest, res.Manifest, head1, incr1); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	head1.Close()
	incr1.Close()

	// Drop a stray untracked file that is NOT in the checkpoint. A
	// re-apply must clean it up.
	stray := filepath.Join(dest, "stray.txt")
	if err := os.WriteFile(stray, []byte("left over from a previous session\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	head2, incr2 := openBundles()
	if err := checkpoint.Apply(ctx, dest, res.Manifest, head2, incr2); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	head2.Close()
	incr2.Close()

	if _, err := os.Stat(stray); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale untracked file survived re-apply: stat err=%v", err)
	}
}

// TestManifest_RoundTripJSON verifies Marshal/UnmarshalManifest are
// stable.
func TestManifest_RoundTripJSON(t *testing.T) {
	t.Parallel()
	want := &checkpoint.Manifest{
		Version:           checkpoint.ManifestVersion,
		CheckpointID:      "ck-1",
		HeadCommit:        "deadbeef",
		HeadRef:           "main",
		IndexTree:         "1111",
		WorktreeTree:      "2222",
		IncrementalCommit: "3333",
		CreatedAt:         time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
		CreatedBy:         "laptop:dev",
	}
	data, err := want.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := checkpoint.UnmarshalManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadCommit != want.HeadCommit ||
		got.HeadRef != want.HeadRef ||
		got.IndexTree != want.IndexTree ||
		got.WorktreeTree != want.WorktreeTree ||
		got.IncrementalCommit != want.IncrementalCommit ||
		!got.CreatedAt.Equal(want.CreatedAt) ||
		got.CreatedBy != want.CreatedBy {
		t.Fatalf("manifest round-trip mismatch:\n want %+v\n got  %+v", want, got)
	}
}

// TestSnapshotMatchesBuildManifest pins the invariant that
// Snapshot() and Build().Manifest agree on all 4 content SHAs for
// the same working tree. Both share an internal helper, so this
// guards against future drift if either path is edited.
func TestSnapshotMatchesBuildManifest(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repo := setupRepo(t, ctx)

	writeFile(t, repo, "main.go", "package main\n\nfunc main(){}\n")
	gitMustRun(t, ctx, repo, "add", "main.go")
	gitMustRun(t, ctx, repo, "commit", "-m", "initial")
	writeFile(t, repo, "staged.txt", "this is staged\n")
	gitMustRun(t, ctx, repo, "add", "staged.txt")
	writeFile(t, repo, "main.go", "package main\n\n// edited\nfunc main(){}\n")
	writeFile(t, repo, "untracked.md", "# Notes\n")

	b := checkpoint.NewBuilder(repo, "test:laptop")
	snap, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	res, err := b.Build(ctx, "ck-parity")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer res.Cleanup()

	if snap.HeadCommit != res.Manifest.HeadCommit {
		t.Errorf("HeadCommit drift: snap=%q build=%q", snap.HeadCommit, res.Manifest.HeadCommit)
	}
	if snap.HeadRef != res.Manifest.HeadRef {
		t.Errorf("HeadRef drift: snap=%q build=%q", snap.HeadRef, res.Manifest.HeadRef)
	}
	if snap.IndexTree != res.Manifest.IndexTree {
		t.Errorf("IndexTree drift: snap=%q build=%q", snap.IndexTree, res.Manifest.IndexTree)
	}
	if snap.WorktreeTree != res.Manifest.WorktreeTree {
		t.Errorf("WorktreeTree drift: snap=%q build=%q", snap.WorktreeTree, res.Manifest.WorktreeTree)
	}
}

// roundTripAndAssert builds a checkpoint of repo, snapshots its state,
// blows away the working tree, restores, and asserts equivalence.
func roundTripAndAssert(t *testing.T, ctx context.Context, repo, checkpointID string) {
	t.Helper()

	wantHead := strings.TrimSpace(gitMustOutput(t, ctx, repo, "rev-parse", "HEAD"))
	wantRef := branchOrEmpty(t, ctx, repo)
	wantStatus := strings.TrimSpace(gitMustOutput(t, ctx, repo, "status", "--porcelain=v1", "-uall"))
	wantFiles := snapshotFiles(t, repo)
	wantIndexTree := strings.TrimSpace(gitMustOutput(t, ctx, repo, "write-tree"))

	builder := checkpoint.NewBuilder(repo, "test:laptop")
	res, err := builder.Build(ctx, checkpointID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer res.Cleanup()

	if res.Manifest.HeadCommit != wantHead {
		t.Fatalf("manifest.HeadCommit = %q, want %q", res.Manifest.HeadCommit, wantHead)
	}
	if res.Manifest.HeadRef != wantRef {
		t.Fatalf("manifest.HeadRef = %q, want %q", res.Manifest.HeadRef, wantRef)
	}
	if res.Manifest.IndexTree != wantIndexTree {
		t.Fatalf("manifest.IndexTree = %q, want %q", res.Manifest.IndexTree, wantIndexTree)
	}

	headBundle, err := os.Open(res.HeadCommitBundle)
	if err != nil {
		t.Fatal(err)
	}
	defer headBundle.Close()
	incrBundle, err := os.Open(res.IncrementalBundle)
	if err != nil {
		t.Fatal(err)
	}
	defer incrBundle.Close()

	dest := t.TempDir()
	if err := checkpoint.Apply(ctx, dest, res.Manifest, headBundle, incrBundle); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	gotHead := strings.TrimSpace(gitMustOutput(t, ctx, dest, "rev-parse", "HEAD"))
	if gotHead != wantHead {
		t.Fatalf("restored HEAD = %q, want %q", gotHead, wantHead)
	}
	gotRef := branchOrEmpty(t, ctx, dest)
	if gotRef != wantRef {
		t.Fatalf("restored ref = %q, want %q", gotRef, wantRef)
	}
	gotIndexTree := strings.TrimSpace(gitMustOutput(t, ctx, dest, "write-tree"))
	if gotIndexTree != wantIndexTree {
		t.Fatalf("restored indexTree = %q, want %q", gotIndexTree, wantIndexTree)
	}
	gotStatus := strings.TrimSpace(gitMustOutput(t, ctx, dest, "status", "--porcelain=v1", "-uall"))
	if gotStatus != wantStatus {
		t.Fatalf("restored status mismatch:\n want:\n%s\n got:\n%s", wantStatus, gotStatus)
	}
	gotFiles := snapshotFiles(t, dest)
	if !filesEqual(wantFiles, gotFiles) {
		t.Fatalf("file content mismatch:\n want: %v\n got:  %v", wantFiles, gotFiles)
	}
}

// setupRepo creates a fresh git repo with deterministic identity.
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

func branchOrEmpty(t *testing.T, ctx context.Context, dir string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "symbolic-ref", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// snapshotFiles walks the working tree (not .git) and returns a map of
// relpath → content.
func snapshotFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if rel == ".git" || strings.HasPrefix(rel, ".git/") {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func filesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}

// seedRandomTree creates a few commits + a few staged + a few
// untracked files of random content, deterministic per seed.
func seedRandomTree(t *testing.T, ctx context.Context, dir string, rng *mathrand.Rand, n int) {
	t.Helper()
	// One commit with a few base files.
	for i := 0; i < n/2; i++ {
		writeFile(t, dir, fmt.Sprintf("base-%d.txt", i), randString(rng, 32+rng.IntN(64)))
	}
	gitMustRun(t, ctx, dir, "add", ".")
	gitMustRun(t, ctx, dir, "commit", "-m", "base")
	// Staged additions.
	for i := 0; i < n/4; i++ {
		writeFile(t, dir, fmt.Sprintf("staged-%d.txt", i), randString(rng, 16+rng.IntN(48)))
	}
	gitMustRun(t, ctx, dir, "add", ".")
	// Unstaged modifications.
	for i := 0; i < n/4; i++ {
		writeFile(t, dir, fmt.Sprintf("base-%d.txt", i), randString(rng, 32))
	}
	// Untracked files.
	for i := 0; i < n/4; i++ {
		writeFile(t, dir, fmt.Sprintf("untracked-%d.md", i), randString(rng, 24))
	}
}

func randString(rng *mathrand.Rand, n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte(rng.UintN(256))
	}
	return hex.EncodeToString(buf) + "\n"
}
