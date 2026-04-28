package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is a tiny in-memory AgentStore for unit tests.
type fakeStore struct {
	mu  sync.Mutex
	got map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{got: map[string]string{}} }

func (f *fakeStore) key(repoKey, branch string) string { return repoKey + "\x00" + branch }

func (f *fakeStore) LoadSyncStateTip(repoKey, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[f.key(repoKey, branch)], nil
}

func (f *fakeStore) UpsertSyncStateTip(repoKey, branch, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got[f.key(repoKey, branch)] = sha
	return nil
}

// makeLocalRepo creates a non-bare repo at dir, configures origin to
// point at remoteURL (we just need the URL string — we don't push),
// and commits one file on the named branch. Returns the branch tip.
func makeLocalRepo(t *testing.T, dir, branch, remoteURL, fileName, contents string) string {
	t.Helper()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", branch, dir)
	run("-C", dir, "remote", "add", "origin", remoteURL)
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(contents), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("-C", dir, "add", fileName)
	run("-C", dir, "commit", "-m", "init")
	out, err := exec.Command("git", "-C", dir, "rev-parse", branch).Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return string(out[:len(out)-1])
}

// receivedBundles collects what the test server sees.
type receivedBundles struct {
	mu   sync.Mutex
	hits []receivedHit
}

type receivedHit struct {
	RepoKey   string
	Branch    string
	TipSHA    string
	BaseSHA   string
	RemoteURL string
	Bytes     int
}

func TestAgent_PushesNewBranches(t *testing.T) {
	root, err := NewMirrorRoot(t.TempDir())
	if err != nil {
		t.Fatalf("NewMirrorRoot: %v", err)
	}
	recv := NewReceiver(root, nil, nil)

	rcvd := &receivedBundles{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inline mux: only handle the bundle path.
		// Match "/sync/repos/{key}/bundle"
		path := r.URL.Path
		const prefix = "/sync/repos/"
		if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, "/bundle") {
			http.NotFound(w, r)
			return
		}
		repoKey := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/bundle")
		if r.Header.Get("Authorization") != "Bearer xyz" {
			http.Error(w, "unauthorized", 401)
			return
		}
		err := recv.ReceiveBundle(r.Context(), ReceiveBundleRequest{
			RepoKey:   repoKey,
			RemoteURL: r.Header.Get("X-Clank-Remote-URL"),
			Branch:    r.Header.Get("X-Clank-Branch"),
			TipSHA:    r.Header.Get("X-Clank-Tip-SHA"),
			BaseSHA:   r.Header.Get("X-Clank-Base-SHA"),
			Bundle:    r.Body,
		})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		rcvd.mu.Lock()
		rcvd.hits = append(rcvd.hits, receivedHit{
			RepoKey:   repoKey,
			Branch:    r.Header.Get("X-Clank-Branch"),
			TipSHA:    r.Header.Get("X-Clank-Tip-SHA"),
			BaseSHA:   r.Header.Get("X-Clank-Base-SHA"),
			RemoteURL: r.Header.Get("X-Clank-Remote-URL"),
		})
		rcvd.mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)

	// One local repo.
	const remoteURL = "https://github.com/example/foo.git"
	repoDir := t.TempDir()
	tip := makeLocalRepo(t, repoDir, "feat/x", remoteURL, "marker.txt", "hi\n")

	store := newFakeStore()
	pusher := NewPusher(srv.URL, "xyz", nil)
	agent, err := NewAgent(AgentOptions{
		Repos:    []string{repoDir},
		Pusher:   pusher,
		Store:    store,
		Interval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	agent.Start(ctx)
	t.Cleanup(agent.Stop)

	// Wait for the initial scan to push.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rcvd.mu.Lock()
		n := len(rcvd.hits)
		rcvd.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	rcvd.mu.Lock()
	hits := append([]receivedHit(nil), rcvd.hits...)
	rcvd.mu.Unlock()

	if len(hits) < 1 {
		t.Fatalf("agent did not push any bundle")
	}
	repoKey := RepoKey(remoteURL)
	if hits[0].RepoKey != repoKey {
		t.Errorf("repo_key: want %s, got %s", repoKey, hits[0].RepoKey)
	}
	if hits[0].Branch != "feat/x" {
		t.Errorf("branch: want feat/x, got %s", hits[0].Branch)
	}
	if hits[0].TipSHA != tip {
		t.Errorf("tip: want %s, got %s", tip, hits[0].TipSHA)
	}
	if hits[0].BaseSHA != "" {
		t.Errorf("first push base should be empty, got %s", hits[0].BaseSHA)
	}

	// Idempotent: with no new commits, the next scan should not produce
	// a duplicate hit.
	priorN := len(hits)
	time.Sleep(150 * time.Millisecond)
	rcvd.mu.Lock()
	n := len(rcvd.hits)
	rcvd.mu.Unlock()
	if n != priorN {
		t.Errorf("agent re-pushed unchanged branch: prior=%d now=%d", priorN, n)
	}

	// New commit on the branch — agent should push an incremental
	// bundle whose base is the previous tip.
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if err := os.WriteFile(filepath.Join(repoDir, "marker.txt"), []byte("hi v2\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	commit := exec.Command("git", "-C", repoDir, "commit", "-am", "second")
	commit.Env = gitEnv
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	out2, err := exec.Command("git", "-C", repoDir, "rev-parse", "feat/x").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	tip2 := string(out2[:len(out2)-1])

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rcvd.mu.Lock()
		n := len(rcvd.hits)
		rcvd.mu.Unlock()
		if n > priorN {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rcvd.mu.Lock()
	hits = append([]receivedHit(nil), rcvd.hits...)
	rcvd.mu.Unlock()
	if len(hits) <= priorN {
		t.Fatalf("agent did not push after new commit")
	}
	last := hits[len(hits)-1]
	if last.TipSHA != tip2 {
		t.Errorf("incremental tip: want %s, got %s", tip2, last.TipSHA)
	}
	if last.BaseSHA != tip {
		t.Errorf("incremental base: want %s, got %s", tip, last.BaseSHA)
	}
}

// TestAgent_StopWithoutStart_NoOp pins the lifecycle guard: a Stop
// call before Start must not block on doneCh forever. Used to be a
// real footgun — test setup paths that constructed an Agent and
// then deferred Stop() would hang the test process if Start was
// skipped.
func TestAgent_StopWithoutStart_NoOp(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	agent, err := NewAgent(AgentOptions{
		Pusher: NewPusher("http://unused.invalid", "tkn", nil),
		Store:  store,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	done := make(chan struct{})
	go func() {
		agent.Stop()
		close(done)
	}()
	select {
	case <-done:
		// success — Stop returned without a Start
	case <-time.After(2 * time.Second):
		t.Fatal("Stop without Start blocked > 2s")
	}
}

// TestAgent_DoubleStart_NoOp pins the second guard: a stray second
// Start call must not spawn another goroutine, otherwise the second
// `defer close(doneCh)` panics on close-of-closed-channel and
// crashes the daemon.
func TestAgent_DoubleStart_NoOp(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	agent, err := NewAgent(AgentOptions{
		Pusher:   NewPusher("http://unused.invalid", "tkn", nil),
		Store:    store,
		Interval: time.Hour, // we don't want a scan during this test
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agent.Start(ctx)
	// Second call must be a no-op. Without the guard this panics
	// on the inner `defer close(doneCh)` from the second goroutine.
	agent.Start(ctx)

	// And shutdown still works cleanly.
	done := make(chan struct{})
	go func() {
		agent.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop after double Start blocked > 2s")
	}
}
