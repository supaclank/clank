package clankcli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// fakeRemote serves GET /v1/worktrees/{id} with a configurable
// payload so we can exercise checkParity's branches without spinning
// up the full sync server.
type fakeRemote struct {
	srv  *httptest.Server
	resp string // raw JSON body
	code int    // HTTP status
}

func newFakeRemote(t *testing.T) *fakeRemote {
	t.Helper()
	f := &fakeRemote{code: http.StatusOK}
	mx := http.NewServeMux()
	mx.HandleFunc("GET /v1/worktrees/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.code)
		_, _ = w.Write([]byte(f.resp))
	})
	f.srv = httptest.NewServer(mx)
	t.Cleanup(f.srv.Close)
	return f
}

func newRemoteClient(t *testing.T, baseURL string) *daemonclient.Client {
	t.Helper()
	return daemonclient.NewTCPClient(baseURL, "test-token")
}

func worktreeJSON(id, ownerKind string, snap *checkpoint.Snapshot) string {
	type ckMeta struct {
		HeadCommit   string `json:"head_commit"`
		HeadRef      string `json:"head_ref,omitempty"`
		IndexTree    string `json:"index_tree"`
		WorktreeTree string `json:"worktree_tree"`
	}
	body := struct {
		ID            string  `json:"id"`
		OwnerKind     string  `json:"owner_kind"`
		LatestSynced  string  `json:"latest_synced_checkpoint,omitempty"`
		LatestMeta    *ckMeta `json:"latest_checkpoint_metadata,omitempty"`
	}{ID: id, OwnerKind: ownerKind}
	if snap != nil {
		body.LatestSynced = "ck-test"
		body.LatestMeta = &ckMeta{
			HeadCommit:   snap.HeadCommit,
			HeadRef:      snap.HeadRef,
			IndexTree:    snap.IndexTree,
			WorktreeTree: snap.WorktreeTree,
		}
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func TestCheckParity_InSyncLocalOwned(t *testing.T) {
	t.Parallel()
	snap := &checkpoint.Snapshot{
		HeadCommit:   "abc",
		HeadRef:      "main",
		IndexTree:    "idx",
		WorktreeTree: "wt",
	}
	f := newFakeRemote(t)
	f.resp = worktreeJSON("wt-id", "local", snap)
	dc := newRemoteClient(t, f.srv.URL)

	res, err := checkParity(context.Background(), dc, "wt-id", snap)
	if err != nil {
		t.Fatalf("checkParity: %v", err)
	}
	if res.OwnerKind != "local" {
		t.Errorf("OwnerKind = %q, want local", res.OwnerKind)
	}
	if !res.InSync {
		t.Errorf("InSync should be true when SHAs match")
	}
	if !res.HasCheckpoint {
		t.Errorf("HasCheckpoint should be true when remote returned a snapshot")
	}
}

func TestCheckParity_DivergedRemoteOwned(t *testing.T) {
	t.Parallel()
	localSnap := &checkpoint.Snapshot{HeadCommit: "abc", IndexTree: "idx", WorktreeTree: "wt"}
	remoteSnap := &checkpoint.Snapshot{HeadCommit: "def", IndexTree: "idx2", WorktreeTree: "wt2"}
	f := newFakeRemote(t)
	f.resp = worktreeJSON("wt-id", "remote", remoteSnap)
	dc := newRemoteClient(t, f.srv.URL)

	res, err := checkParity(context.Background(), dc, "wt-id", localSnap)
	if err != nil {
		t.Fatalf("checkParity: %v", err)
	}
	if res.OwnerKind != "remote" || res.InSync {
		t.Errorf("expected remote/!InSync, got %+v", res)
	}
	if res.RemoteHead != "def" || res.LocalHead != "abc" {
		t.Errorf("head SHAs mismatched: %+v", res)
	}
}

func TestCheckParity_NoCheckpointYet(t *testing.T) {
	t.Parallel()
	snap := &checkpoint.Snapshot{HeadCommit: "abc", IndexTree: "idx", WorktreeTree: "wt"}
	f := newFakeRemote(t)
	// remote knows the worktree but no checkpoint pushed yet —
	// latest_checkpoint_metadata is omitted from the JSON.
	f.resp = worktreeJSON("wt-id", "local", nil)
	dc := newRemoteClient(t, f.srv.URL)

	res, err := checkParity(context.Background(), dc, "wt-id", snap)
	if err != nil {
		t.Fatalf("checkParity: %v", err)
	}
	if res.HasCheckpoint {
		t.Errorf("HasCheckpoint should be false when remote has no checkpoint")
	}
	if res.InSync {
		t.Errorf("InSync should be false when no remote checkpoint exists")
	}
}

func TestCheckParity_WorktreeNotFound(t *testing.T) {
	t.Parallel()
	snap := &checkpoint.Snapshot{HeadCommit: "abc"}
	f := newFakeRemote(t)
	f.code = http.StatusNotFound
	f.resp = "not found"
	dc := newRemoteClient(t, f.srv.URL)

	res, err := checkParity(context.Background(), dc, "missing-wt", snap)
	if err != nil {
		t.Fatalf("404 should not produce an error; got: %v", err)
	}
	if !res.RemoteNotFound {
		t.Errorf("RemoteNotFound should be true on 404")
	}
}

func TestCheckParity_HardError(t *testing.T) {
	t.Parallel()
	snap := &checkpoint.Snapshot{HeadCommit: "abc"}
	f := newFakeRemote(t)
	f.code = http.StatusInternalServerError
	f.resp = "boom"
	dc := newRemoteClient(t, f.srv.URL)

	_, err := checkParity(context.Background(), dc, "wt-id", snap)
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, daemonclient.ErrWorktreeNotFound) {
		t.Errorf("500 should not surface as WorktreeNotFound: %v", err)
	}
}

func TestCheckParity_NilSnapshot(t *testing.T) {
	t.Parallel()
	f := newFakeRemote(t)
	dc := newRemoteClient(t, f.srv.URL)
	if _, err := checkParity(context.Background(), dc, "wt-id", nil); err == nil {
		t.Fatal("nil snapshot should error")
	}
}

func TestIsGitRepo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	missing := filepath.Join(root, "does-not-exist")
	if ok, err := isGitRepo(missing); err != nil || ok {
		t.Errorf("missing path: got (%v, %v), want (false, nil)", ok, err)
	}

	bare := filepath.Join(root, "bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := isGitRepo(bare); err != nil || ok {
		t.Errorf("non-git dir: got (%v, %v), want (false, nil)", ok, err)
	}

	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := isGitRepo(repo); err != nil || !ok {
		t.Errorf("git dir: got (%v, %v), want (true, nil)", ok, err)
	}
}
