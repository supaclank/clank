package hostmux_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostmux "github.com/acksell/clank/internal/host/mux"
	"github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// TestSyncSessionsBuild_EmptyWorktree exercises the build endpoint
// against a worktree with no sessions. Verifies the mux wiring and
// response shape without needing opencode.
func TestSyncSessionsBuild_EmptyWorktree(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	mux := hostmux.New(svc, nil)
	srv := httptest.NewServer(mux.Handler())
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"worktree_id":"wt-empty","checkpoint_id":"ck-1"}`)
	resp, err := http.Post(srv.URL+"/sync/sessions/build", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d: %s", resp.StatusCode, buf)
	}

	var out struct {
		BuildID string                    `json:"build_id"`
		Entries []checkpoint.SessionEntry `json:"entries"`
		Skipped []host.SkippedSession     `json:"skipped"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.BuildID == "" {
		t.Error("expected non-empty build_id")
	}
	if len(out.Entries) != 0 {
		t.Errorf("expected 0 entries for empty worktree, got %d", len(out.Entries))
	}
}

// TestSyncSessionsBuild_RejectsMissingArgs covers input-validation.
func TestSyncSessionsBuild_RejectsMissingArgs(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	mux := hostmux.New(svc, nil)
	srv := httptest.NewServer(mux.Handler())
	t.Cleanup(srv.Close)

	cases := []struct{ body string }{
		{`{"worktree_id":"","checkpoint_id":"ck"}`},
		{`{"worktree_id":"wt","checkpoint_id":""}`},
		{`{}`},
		{`not json`},
	}
	for _, c := range cases {
		resp, err := http.Post(srv.URL+"/sync/sessions/build", "application/json", strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400", c.body, resp.StatusCode)
		}
	}
}

// TestSyncSessionsApply_RejectsMissingArgs covers input-validation for the apply endpoint.
func TestSyncSessionsApply_RejectsMissingArgs(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	mux := hostmux.New(svc, nil)
	srv := httptest.NewServer(mux.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/sync/sessions/apply-from-urls", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing args: status %d, want 400", resp.StatusCode)
	}
}

// TestSyncSessionsApply_BadManifest covers manifest-rejection logic.
// Spins up an httptest manifest server returning malformed JSON.
func TestSyncSessionsApply_BadManifest(t *testing.T) {
	t.Parallel()

	manifestSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))
	t.Cleanup(manifestSrv.Close)

	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	mux := hostmux.New(svc, nil)
	srv := httptest.NewServer(mux.Handler())
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{
		"worktree_id":          "wt",
		"session_manifest_url": manifestSrv.URL,
		"session_blob_urls":    map[string]string{},
	})
	resp, err := http.Post(srv.URL+"/sync/sessions/apply-from-urls", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for malformed manifest", resp.StatusCode)
	}
}

// TestSyncSessionsApply_SkipsMalformedBlob is the regression for a single
// corrupt session blob wedging an entire worktree's sessions. A truncated
// export fails RewriteImportBlob with "unexpected end of JSON input"; the
// import must skip that session and carry on (returning 204), not abort the
// whole batch with a 500 — which previously left the worktree stuck "behind"
// and hid every other session in it.
func TestSyncSessionsApply_SkipsMalformedBlob(t *testing.T) {
	t.Parallel()

	// One server serves the manifest and both (corrupt) session blobs.
	manifest := &checkpoint.SessionManifest{
		Version:      checkpoint.SessionManifestVersion,
		CheckpointID: "ck-1",
		Sessions: []checkpoint.SessionEntry{
			{SessionID: "01HSESSIONAAA0000000000000", ExternalID: "ses_a", Backend: agent.BackendOpenCode},
			{SessionID: "01HSESSIONBBB0000000000000", ExternalID: "ses_b", Backend: agent.BackendOpenCode},
		},
	}
	manifestBytes, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	blobSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest" {
			w.Write(manifestBytes)
			return
		}
		w.Write([]byte("{")) // truncated → "unexpected end of JSON input"
	}))
	t.Cleanup(blobSrv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{agent.BackendOpenCode: &noopBackendManager{}},
		SessionsStore:   st,
	})
	t.Cleanup(svc.Shutdown)
	mux := hostmux.New(svc, nil)
	srv := httptest.NewServer(mux.Handler())
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{
		"worktree_id":          "wt-poison",
		"session_manifest_url": blobSrv.URL + "/manifest",
		"session_blob_urls":    map[string]string{"ses_a": blobSrv.URL + "/blob/a", "ses_b": blobSrv.URL + "/blob/b"},
	})
	resp, err := http.Post(srv.URL+"/sync/sessions/apply-from-urls", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The crux: a malformed blob is skipped, not fatal — the batch succeeds.
	if resp.StatusCode != http.StatusNoContent {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want 204 (malformed blob must skip, not wedge the worktree): %s", resp.StatusCode, buf)
	}
	// Both were malformed, so nothing imported — and critically the loop
	// reached the second session rather than aborting at the first.
	infos, err := svc.ListSessionMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Errorf("registered %d sessions, want 0 (both blobs malformed)", len(infos))
	}
}

// noopBackendManager mirrors the host_test fixture. Duplicated here
// because the mux test package is hostmux_test, not host_test —
// they don't share private symbols. Kept minimal.
type noopBackendManager struct{}

func (m *noopBackendManager) Init(_ context.Context, _ func() ([]string, error)) error { return nil }
func (m *noopBackendManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	return &noopBackend{}, nil
}
func (m *noopBackendManager) Shutdown() {}

type noopBackend struct{}

func (b *noopBackend) Open(_ context.Context) error                              { return nil }
func (b *noopBackend) OpenAndSend(_ context.Context, _ agent.SendMessageOpts) error { return nil }
func (b *noopBackend) Send(_ context.Context, _ agent.SendMessageOpts) error     { return nil }
func (b *noopBackend) Abort(_ context.Context) error                             { return nil }
func (b *noopBackend) Stop() error                                               { return nil }
func (b *noopBackend) Events() <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (b *noopBackend) Status() agent.SessionStatus                            { return agent.StatusIdle }
func (b *noopBackend) SessionID() string                                      { return "stub" }
func (b *noopBackend) Messages(_ context.Context) ([]agent.MessageData, error) { return nil, nil }
func (b *noopBackend) Revert(_ context.Context, _ string) error               { return nil }
func (b *noopBackend) Fork(_ context.Context, _ string) (agent.ForkResult, error) {
	return agent.ForkResult{}, nil
}
func (b *noopBackend) RespondPermission(_ context.Context, _ string, _ bool, _ string) error { return nil }
