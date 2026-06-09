package syncclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	syncclient "github.com/acksell/clank/pkg/sync/client"
)

// TestPullWorktree_DecodesResponse drives PullWorktree against an
// httptest server standing in for the gateway pull endpoint: it asserts
// the request lands on the right method+path with the bearer token and
// that the presigned URLs decode into the result.
func TestPullWorktree_DecodesResponse(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{
			"checkpoint_id": "ck-9",
			"manifest_url": "https://s3/manifest",
			"head_bundles": [{"tip_sha": "h0", "get_url": "https://s3/head0"}, {"tip_sha": "h1", "base_sha": "h0", "get_url": "https://s3/head1"}],
			"uncommitted_url": "https://s3/incr",
			"session_manifest_url": "https://s3/sessions.json",
			"session_blob_urls": {"sess-1": "https://s3/sess-1"}
		}`))
	}))
	defer srv.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: srv.URL, AuthToken: "tok-1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := cli.PullWorktree(context.Background(), "wt-abc", "")
	if err != nil {
		t.Fatalf("PullWorktree: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/worktrees/wt-abc/pull" {
		t.Errorf("path = %s, want /v1/worktrees/wt-abc/pull", gotPath)
	}
	if gotAuth != "Bearer tok-1" {
		t.Errorf("auth = %q, want %q", gotAuth, "Bearer tok-1")
	}
	if res.CheckpointID != "ck-9" || res.ManifestURL != "https://s3/manifest" {
		t.Errorf("decoded result mismatch: %+v", res)
	}
	if res.SessionManifestURL != "https://s3/sessions.json" || res.SessionBlobURLs["sess-1"] != "https://s3/sess-1" {
		t.Errorf("session URLs not decoded: %+v", res)
	}
	if len(res.HeadBundles) != 2 || res.HeadBundles[0].GetURL != "https://s3/head0" || res.HeadBundles[1].BaseSHA != "h0" {
		t.Errorf("head chain not decoded: %+v", res.HeadBundles)
	}
}

// TestPullWorktree_EmptyHeadChainAccepted pins that a 2xx with an empty head
// chain is accepted; only manifest + uncommitted URLs are required.
func TestPullWorktree_EmptyHeadChainAccepted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"checkpoint_id": "ck-9",
			"manifest_url": "https://s3/manifest",
			"head_bundles": [],
			"uncommitted_url": "https://s3/incr"
		}`))
	}))
	defer srv.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := cli.PullWorktree(context.Background(), "wt-abc", "deadbeefcafe")
	if err != nil {
		t.Fatalf("empty head chain should be accepted, got %v", err)
	}
	if len(res.HeadBundles) != 0 {
		t.Errorf("HeadBundles = %+v, want empty", res.HeadBundles)
	}
	if res.ManifestURL != "https://s3/manifest" || res.UncommittedURL != "https://s3/incr" {
		t.Errorf("decoded result mismatch: %+v", res)
	}
}

func TestPullWorktree_RequiresWorktreeID(t *testing.T) {
	t.Parallel()
	cli, err := syncclient.New(syncclient.Config{BaseURL: "https://example.com"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := cli.PullWorktree(context.Background(), "", ""); err == nil {
		t.Fatal("empty worktreeID should error")
	}
}

func TestPullWorktree_ErrorStatusSurfaces(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sprite build: boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = cli.PullWorktree(context.Background(), "wt-abc", "")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected a 502 error to surface, got %v", err)
	}
}

// TestPullWorktree_IncompleteResponseRejected pins that a 2xx missing
// the bundle URLs is treated as a failure rather than handed to the
// applier, which would fail more confusingly downstream.
func TestPullWorktree_IncompleteResponseRejected(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"checkpoint_id": "ck-9"}`))
	}))
	defer srv.Close()

	cli, err := syncclient.New(syncclient.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = cli.PullWorktree(context.Background(), "wt-abc", "")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected an incomplete-response error, got %v", err)
	}
}
