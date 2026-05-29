package clankcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
	clanksync "github.com/acksell/clank/pkg/sync"
	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// TestPushMigrate_RefusesWithoutDiscardRemote pins the existing safety
// guard: on a remote-owned + diverged worktree, `push -m` with no
// --discard-remote (and no --force) must refuse with the styled
// options block. Regression for the case the user just experienced —
// where the block must now suggest `--discard-remote` first, with
// --force as a legacy alias.
func TestPushMigrate_RefusesWithoutDiscardRemote(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	stdout := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(&bytes.Buffer{})

	parity := parityResult{
		OwnerKind: string(daemonclient.OwnerKindRemote),
		InSync:    false,
	}
	err := runPushMigrate(cmd, context.Background(), newPhaseTimer(false), nil, nil, "", "wt-X", parity, false)
	if err == nil {
		t.Fatal("expected refusal error, got nil")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error should mention refusal: %v", err)
	}
	out := stripANSI(stdout.String())
	for _, want := range []string{
		"Cannot migrate",
		"clank pull -m",
		"clank push -m --discard-remote",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in stdout:\n%s", want, out)
		}
	}
}

// TestPushMigrate_DiscardRemoteCallsReclaim is the headline regression
// test for the bug the user reported: `clank push -m --force` (now
// also --discard-remote) on a remote-owned worktree used to fail
// immediately with a 403 because the reclaim step was missing. After
// the fix the reclaim endpoint MUST be hit before the checkpoint POST.
//
// The test stubs a fake remote that:
//   - returns a remote-owned worktree on GET /v1/worktrees/{id}
//     (used by the runPushMigrate "fresh fetch before reclaim" step)
//   - flips to local-owned on POST /v1/worktrees/{id}/owner
//   - counts hits so we can assert reclaim was called exactly once
//
// We don't drive the full push+migrate pipeline (that needs a local
// clankd socket for the opencode compat check and the session leg);
// once reclaim hits we let the next step error out. The assertion is
// "reclaim was called", which is what the bug was about.
func TestPushMigrate_DiscardRemoteCallsReclaim(t *testing.T) {
	t.Parallel()
	var (
		mu          sync.Mutex
		reclaimHits int
	)
	mx := http.NewServeMux()
	mx.HandleFunc("GET /v1/worktrees/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(daemonclient.WorktreeInfo{
			ID:        r.PathValue("id"),
			OwnerKind: string(daemonclient.OwnerKindRemote),
			OwnerID:   "host-X",
		})
	})
	mx.HandleFunc("POST /v1/worktrees/{id}/owner", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reclaimHits++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(daemonclient.WorktreeInfo{
			ID:        r.PathValue("id"),
			OwnerKind: string(daemonclient.OwnerKindLocal),
			OwnerID:   "",
		})
	})
	srv := httptest.NewServer(mx)
	defer srv.Close()

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	stdout := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(&bytes.Buffer{})

	dc := daemonclient.NewTCPClient(srv.URL, "tok")
	parity := parityResult{
		OwnerKind: string(daemonclient.OwnerKindRemote),
		InSync:    false,
	}
	// nil cli is fine — we never reach PushCheckpoint in this test
	// (the version-check or session-leg step fails first because there
	// is no local clankd). The assertion is on reclaim having happened.
	_ = runPushMigrate(cmd, context.Background(), newPhaseTimer(false), nil, dc, "", "wt-1", parity, true)

	mu.Lock()
	gotHits := reclaimHits
	mu.Unlock()
	if gotHits != 1 {
		t.Fatalf("reclaim endpoint hits = %d, want exactly 1 (the bug was that it was 0)", gotHits)
	}
	if !strings.Contains(stripANSI(stdout.String()), "Reclaiming ownership") {
		t.Errorf("expected reclaim warning in stdout; got:\n%s", stdout.String())
	}
}

// TestPushMigrate_DiscardRemoteSurfacesOwnerConflict pins the
// race-recovery contract: when /v1/worktrees/{id}/owner returns 409
// (someone else reclaimed first), the CLI surfaces a clear "re-run"
// message rather than silently retrying or dumping the raw HTTP error.
func TestPushMigrate_DiscardRemoteSurfacesOwnerConflict(t *testing.T) {
	t.Parallel()
	mx := http.NewServeMux()
	mx.HandleFunc("GET /v1/worktrees/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(daemonclient.WorktreeInfo{
			ID:        r.PathValue("id"),
			OwnerKind: string(daemonclient.OwnerKindRemote),
			OwnerID:   "host-X",
		})
	})
	mx.HandleFunc("POST /v1/worktrees/{id}/owner", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "owner mismatch (concurrent migration?)", http.StatusConflict)
	})
	srv := httptest.NewServer(mx)
	defer srv.Close()

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	dc := daemonclient.NewTCPClient(srv.URL, "tok")
	parity := parityResult{
		OwnerKind: string(daemonclient.OwnerKindRemote),
		InSync:    false,
	}
	err := runPushMigrate(cmd, context.Background(), newPhaseTimer(false), nil, dc, "", "wt-1", parity, true)
	if err == nil {
		t.Fatal("expected error after 409, got nil")
	}
	if !strings.Contains(err.Error(), "re-run") {
		t.Errorf("error should mention re-run: %v", err)
	}
	if !strings.Contains(err.Error(), "--discard-remote") {
		t.Errorf("error should mention --discard-remote: %v", err)
	}
}

// TestPushNoMigrate_OwnerMismatchShowsBothOptions pins the uniform-UX
// contract: plain `clank push` against a remote-owned worktree must
// render the same styled options block that `push -m`'s refusal shows,
// so users see both reclaim paths (pull -m, push -m --discard-remote)
// regardless of which command they tried first.
func TestPushNoMigrate_OwnerMismatchShowsBothOptions(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*1000*1000*1000)
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate the server's owner-mismatch 403 on /v1/checkpoints.
		// Any path under POST will get this — fine because the test
		// only drives PushCheckpoint and we don't care which sub-call
		// triggers it.
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(clanksync.OwnerMismatchSentinel + ": worktree is owned by remote\n"))
	}))
	defer srv.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: srv.URL, AuthToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}

	repo := setupGitRepoForPush(t, ctx)

	cmd := &cobra.Command{}
	stdout := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(&bytes.Buffer{})

	// parity has !InSync so it proceeds to push; the stub then 403s.
	parity := parityResult{InSync: false}
	pushErr := runPushNoMigrate(cmd, ctx, newPhaseTimer(false), cli, repo, "wt-1", parity)
	if pushErr == nil {
		t.Fatal("expected error from runPushNoMigrate, got nil")
	}
	if !errors.Is(pushErr, syncclient.ErrOwnerMismatch) && !strings.Contains(pushErr.Error(), "refused") {
		t.Fatalf("expected ErrOwnerMismatch or refusal; got %v", pushErr)
	}
	out := stripANSI(stdout.String())
	for _, want := range []string{"clank pull -m", "clank push -m --discard-remote"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in styled options block; got:\n%s", want, out)
		}
	}
}

// setupGitRepoForPush builds a minimal real git repo with one commit so
// checkpoint.NewBuilder.Build can produce real bundles. Reuses git
// from PATH; no mocks.
func setupGitRepoForPush(t *testing.T, ctx context.Context) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--initial-branch=main", "--quiet"},
		{"config", "user.email", "test@clank.local"},
		{"config", "user.name", "clank-test"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "initial"},
	} {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
		}
	}
	return dir
}
