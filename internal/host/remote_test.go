package host

// Integration tests for the pure-git half of the remote-sync flow. They
// exercise runPush/runPull/runResolve/computeStatus against a real local
// bare repo standing in for origin — no GitHub, no mocks (per CLAUDE.md).
// The GitHub resolution in remoteContextFor is covered separately by the
// host mux / manual e2e.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/git"
)

type remoteFixture struct {
	rc   remoteContext
	bare string
}

// newRemoteFixture builds a bare "origin" with one commit on main and a
// working clone pointed at it via remoteContext (auth empty — a local
// path needs none).
func newRemoteFixture(t *testing.T) remoteFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()

	seed := filepath.Join(root, "seed")
	gitRun(t, "", "init", "-q", "-b", "main", seed)
	configGit(t, seed)
	writeFile(t, filepath.Join(seed, "f.txt"), "base")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-qm", "c1")

	bare := filepath.Join(root, "origin.git")
	gitRun(t, "", "clone", "-q", "--bare", seed, bare)

	work := filepath.Join(root, "work")
	gitRun(t, "", "clone", "-q", bare, work)
	configGit(t, work)

	return remoteFixture{
		rc:   remoteContext{workdir: work, branch: "main", owner: "acme", repo: "api", pushURL: bare},
		bare: bare,
	}
}

// advanceRemote pushes a commit (file=content) onto bare's main via a
// throwaway clone, returning the new main SHA. Simulates "the laptop
// pushed".
func (f remoteFixture) advanceRemote(t *testing.T, file, content, msg string) string {
	t.Helper()
	tmp := t.TempDir()
	gitRun(t, "", "clone", "-q", f.bare, tmp)
	configGit(t, tmp)
	writeFile(t, filepath.Join(tmp, file), content)
	gitRun(t, tmp, "add", ".")
	gitRun(t, tmp, "commit", "-qm", msg)
	gitRun(t, tmp, "push", "-q", "origin", "main")
	return gitRun(t, tmp, "rev-parse", "HEAD")
}

func TestRunPush_CommitsDirtyThenPushes(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	writeFile(t, filepath.Join(f.rc.workdir, "new.txt"), "local work")

	res, err := runPush(f.rc)
	if err != nil {
		t.Fatalf("runPush: %v", err)
	}
	if !res.Committed || !res.Pushed {
		t.Fatalf("res = %+v, want Committed && Pushed", res)
	}
	// Worktree is clean and the remote advanced to our HEAD.
	if clean, _ := git.IsClean(f.rc.workdir); !clean {
		t.Error("worktree should be clean after push")
	}
	if got, want := gitRun(t, f.bare, "rev-parse", "main"), res.HeadSHA; got != want {
		t.Errorf("remote main = %s, want pushed HEAD %s", got, want)
	}
}

func TestRunPush_DivergedIsTyped(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	f.advanceRemote(t, "f.txt", "remote v2", "remote") // remote moves ahead
	// Local makes its own commit → diverged.
	writeFile(t, filepath.Join(f.rc.workdir, "f.txt"), "local v2")
	gitRun(t, f.rc.workdir, "commit", "-aqm", "local")

	_, err := runPush(f.rc)
	if !errors.Is(err, ErrRemoteDiverged) {
		t.Fatalf("runPush err = %v, want ErrRemoteDiverged", err)
	}
}

func TestRunPull_FastForward(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	c2 := f.advanceRemote(t, "f.txt", "v2", "remote")

	res, err := runPull(f.rc)
	if err != nil {
		t.Fatalf("runPull: %v", err)
	}
	if !res.FastForwarded || res.State != RemoteStateSynced {
		t.Fatalf("res = %+v, want FastForwarded && synced", res)
	}
	if got := gitRun(t, f.rc.workdir, "rev-parse", "HEAD"); got != c2 {
		t.Errorf("HEAD = %s, want fast-forwarded to %s", got, c2)
	}
}

func TestRunPull_RefusesDirtyTree(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	f.advanceRemote(t, "f.txt", "v2", "remote")
	writeFile(t, filepath.Join(f.rc.workdir, "f.txt"), "uncommitted local edit")

	_, err := runPull(f.rc)
	if !errors.Is(err, ErrWorktreeDirty) {
		t.Fatalf("runPull err = %v, want ErrWorktreeDirty", err)
	}
}

func TestRunPull_DivergedIsTyped(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	f.advanceRemote(t, "a.txt", "remote", "remote")
	writeFile(t, filepath.Join(f.rc.workdir, "b.txt"), "local")
	gitRun(t, f.rc.workdir, "add", ".")
	gitRun(t, f.rc.workdir, "commit", "-qm", "local")

	_, err := runPull(f.rc)
	if !errors.Is(err, ErrRemoteDiverged) {
		t.Fatalf("runPull err = %v, want ErrRemoteDiverged", err)
	}
}

func TestRunResolve_TakeRemoteBacksUpThenResets(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	c2 := f.advanceRemote(t, "f.txt", "remote v2", "remote")
	writeFile(t, filepath.Join(f.rc.workdir, "f.txt"), "local v2")
	gitRun(t, f.rc.workdir, "commit", "-aqm", "local")
	localHead := gitRun(t, f.rc.workdir, "rev-parse", "HEAD")

	res, err := runResolve(f.rc, ResolveTakeRemote)
	if err != nil {
		t.Fatalf("runResolve take_remote: %v", err)
	}
	if res.State != RemoteStateSynced {
		t.Errorf("State = %s, want synced", res.State)
	}
	if got := gitRun(t, f.rc.workdir, "rev-parse", "HEAD"); got != c2 {
		t.Errorf("HEAD = %s, want reset to remote %s", got, c2)
	}
	// The discarded local commit survives under the backup ref.
	if res.BackupRef == "" {
		t.Fatal("expected a BackupRef")
	}
	if got := gitRun(t, f.rc.workdir, "rev-parse", res.BackupRef); got != localHead {
		t.Errorf("backup ref = %s, want discarded local %s", got, localHead)
	}
}

func TestRunResolve_MergeCleanLeavesUnpushedMergeCommit(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	f.advanceRemote(t, "a.txt", "remote", "remote") // different file → auto-mergeable
	writeFile(t, filepath.Join(f.rc.workdir, "b.txt"), "local")
	gitRun(t, f.rc.workdir, "add", ".")
	gitRun(t, f.rc.workdir, "commit", "-qm", "local")

	res, err := runResolve(f.rc, ResolveMerge)
	if err != nil {
		t.Fatalf("runResolve merge: %v", err)
	}
	if res.State != RemoteStateUnpushed {
		t.Errorf("State = %s, want unpushed", res.State)
	}
	if git.IsMerging(f.rc.workdir) {
		t.Error("should not be mid-merge after a clean merge")
	}
	// A real merge commit has a second parent.
	gitRun(t, f.rc.workdir, "rev-parse", "HEAD^2")
}

func TestRunResolve_MergeConflictThenAbort(t *testing.T) {
	t.Parallel()
	f := newRemoteFixture(t)
	f.advanceRemote(t, "f.txt", "remote change", "remote") // same file → conflict
	writeFile(t, filepath.Join(f.rc.workdir, "f.txt"), "local change")
	gitRun(t, f.rc.workdir, "commit", "-aqm", "local")
	preMerge := gitRun(t, f.rc.workdir, "rev-parse", "HEAD")

	res, err := runResolve(f.rc, ResolveMerge)
	if err != nil {
		t.Fatalf("runResolve merge (conflict path returns a result, not an error): %v", err)
	}
	if res.State != RemoteStateConflict {
		t.Errorf("State = %s, want conflict", res.State)
	}
	if !slices.Contains(res.ConflictedFiles, "f.txt") {
		t.Errorf("ConflictedFiles = %v, want f.txt", res.ConflictedFiles)
	}
	if !git.IsMerging(f.rc.workdir) {
		t.Fatal("should be mid-merge after a conflicting merge")
	}

	// Abort restores the pre-merge state.
	if _, err := runResolve(f.rc, ResolveAbort); err != nil {
		t.Fatalf("runResolve abort: %v", err)
	}
	if git.IsMerging(f.rc.workdir) {
		t.Error("still mid-merge after abort")
	}
	if got := gitRun(t, f.rc.workdir, "rev-parse", "HEAD"); got != preMerge {
		t.Errorf("HEAD = %s, want pre-merge %s", got, preMerge)
	}
}

func TestComputeStatus_States(t *testing.T) {
	t.Parallel()

	t.Run("synced", func(t *testing.T) {
		t.Parallel()
		f := newRemoteFixture(t)
		st, err := computeStatus(f.rc)
		if err != nil {
			t.Fatalf("computeStatus: %v", err)
		}
		if st.State != RemoteStateSynced {
			t.Errorf("State = %s, want synced", st.State)
		}
	})

	t.Run("unpushed_dirty", func(t *testing.T) {
		t.Parallel()
		f := newRemoteFixture(t)
		writeFile(t, filepath.Join(f.rc.workdir, "f.txt"), "dirty")
		st, _ := computeStatus(f.rc)
		if st.State != RemoteStateUnpushed || !st.Dirty {
			t.Errorf("State=%s Dirty=%v, want unpushed & dirty", st.State, st.Dirty)
		}
	})

	t.Run("behind", func(t *testing.T) {
		t.Parallel()
		f := newRemoteFixture(t)
		f.advanceRemote(t, "f.txt", "v2", "remote")
		st, _ := computeStatus(f.rc)
		if st.State != RemoteStateBehind || st.Behind != 1 {
			t.Errorf("State=%s Behind=%d, want behind & 1", st.State, st.Behind)
		}
	})

	t.Run("diverged", func(t *testing.T) {
		t.Parallel()
		f := newRemoteFixture(t)
		f.advanceRemote(t, "a.txt", "remote", "remote")
		writeFile(t, filepath.Join(f.rc.workdir, "b.txt"), "local")
		gitRun(t, f.rc.workdir, "add", ".")
		gitRun(t, f.rc.workdir, "commit", "-qm", "local")
		st, _ := computeStatus(f.rc)
		if st.State != RemoteStateDiverged {
			t.Errorf("State = %s, want diverged", st.State)
		}
	})

	t.Run("no_upstream", func(t *testing.T) {
		t.Parallel()
		f := newRemoteFixture(t)
		gitRun(t, f.rc.workdir, "checkout", "-q", "-b", "feature-x")
		f.rc.branch = "feature-x" // never pushed
		st, _ := computeStatus(f.rc)
		if st.State != RemoteStateNoUpstream {
			t.Errorf("State = %s, want no_upstream", st.State)
		}
	})
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func configGit(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "config", "user.email", "t@e.com")
	gitRun(t, dir, "config", "user.name", "t")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
