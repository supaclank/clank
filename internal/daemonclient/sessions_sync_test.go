package daemonclient_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// TestApplySessionCheckpoint_EmptyManifestURLIsNoOp pins the
// fast-path that lets pull --migrate succeed when the sprite had no
// opencode sessions: an empty SessionManifestURL must NOT trigger a
// daemon call at all. Without this, the laptop would 404 on its own
// /sync/sessions/apply-from-urls with a missing manifest URL and
// abort the migration before the commit step.
func TestApplySessionCheckpoint_EmptyManifestURLIsNoOp(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError) // would fail if called
	}))
	t.Cleanup(srv.Close)

	c := daemonclient.NewTCPClient(srv.URL, "")
	if err := c.ApplySessionCheckpoint(context.Background(), "wt-1", "", nil); err != nil {
		t.Fatalf("empty manifest URL must be a silent no-op, got: %v", err)
	}
	if hits != 0 {
		t.Errorf("daemon endpoint was hit %d times for an empty manifest URL", hits)
	}
}

// TestApplySessionCheckpoint_ForwardsBodyToDaemon proves the
// request body shape mirrors what clank-host's
// /sync/sessions/apply-from-urls expects. Catches drift between
// the daemonclient wrapper and the host mux handler — the two are
// on either side of an HTTP boundary so a typo can't be caught
// by the compiler.
func TestApplySessionCheckpoint_ForwardsBodyToDaemon(t *testing.T) {
	t.Parallel()
	var (
		gotPath string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c := daemonclient.NewTCPClient(srv.URL, "")
	urls := map[string]string{
		"01HSESS0001": "https://example.invalid/sig?one=1",
		"01HSESS0002": "https://example.invalid/sig?two=2",
	}
	err := c.ApplySessionCheckpoint(
		context.Background(),
		"wt-forwarding",
		"https://example.invalid/manifest?sig",
		urls,
	)
	if err != nil {
		t.Fatalf("ApplySessionCheckpoint: %v", err)
	}

	if gotPath != "/sync/sessions/apply-from-urls" {
		t.Errorf("daemon path = %q, want /sync/sessions/apply-from-urls", gotPath)
	}
	if got, want := gotBody["worktree_id"], "wt-forwarding"; got != want {
		t.Errorf("worktree_id = %v, want %v", got, want)
	}
	if got, want := gotBody["session_manifest_url"], "https://example.invalid/manifest?sig"; got != want {
		t.Errorf("session_manifest_url = %v, want %v", got, want)
	}
	gotURLs, ok := gotBody["session_blob_urls"].(map[string]any)
	if !ok {
		t.Fatalf("session_blob_urls type = %T, want map", gotBody["session_blob_urls"])
	}
	if len(gotURLs) != 2 {
		t.Errorf("session_blob_urls len = %d, want 2", len(gotURLs))
	}
	for k, want := range urls {
		if gotURLs[k] != want {
			t.Errorf("session_blob_urls[%q] = %v, want %v", k, gotURLs[k], want)
		}
	}
}

// TestApplySessionCheckpoint_PropagatesDaemonError makes sure a
// non-2xx response from clank-host bubbles up so pull --migrate
// aborts before the ownership-flip step.
func TestApplySessionCheckpoint_PropagatesDaemonError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"session_import_failed","error":"opencode import: bad blob"}`))
	}))
	t.Cleanup(srv.Close)

	c := daemonclient.NewTCPClient(srv.URL, "")
	err := c.ApplySessionCheckpoint(
		context.Background(),
		"wt-err",
		"https://example.invalid/manifest",
		map[string]string{"01H": "https://example.invalid/sig"},
	)
	if err == nil {
		t.Fatal("expected error when daemon returns 500")
	}
}

// TestApplySessionCheckpoint_RejectsEmptyWorktreeID — input validation.
func TestApplySessionCheckpoint_RejectsEmptyWorktreeID(t *testing.T) {
	t.Parallel()
	c := daemonclient.NewTCPClient("http://unused.invalid", "")
	if err := c.ApplySessionCheckpoint(context.Background(), "", "https://x/m", nil); err == nil {
		t.Fatal("expected error for empty worktreeID")
	}
}
