package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// fakeClankHost stands in for the local clank-host subprocess that
// the laptop's gateway proxies to via the local provisioner. It
// records each request so tests can assert routing decisions.
type fakeClankHost struct {
	srv      *httptest.Server
	requests int64
	mu       sync.Mutex
	// sessions[id] is the SessionInfo returned for GET /sessions/{id}.
	// Absent entries return 404. A wildcard "" key returns the same
	// payload for any sessionID — useful when the test doesn't care
	// which id was requested.
	sessions map[string]string

	// list is the JSON body returned for GET /sessions.
	list string
}

func newFakeClankHost(t *testing.T) *fakeClankHost {
	t.Helper()
	f := &fakeClankHost{sessions: map[string]string{}}
	mx := http.NewServeMux()
	mx.HandleFunc("GET /sessions", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&f.requests, 1)
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		body := f.list
		f.mu.Unlock()
		if body == "" {
			body = `[]`
		}
		_, _ = w.Write([]byte(body))
	})
	mx.HandleFunc("GET /sessions/search", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&f.requests, 1)
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		body := f.list
		f.mu.Unlock()
		if body == "" {
			body = `[]`
		}
		_, _ = w.Write([]byte(body))
	})
	mx.HandleFunc("GET /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&f.requests, 1)
		id := r.PathValue("id")
		f.mu.Lock()
		body, ok := f.sessions[id]
		if !ok {
			body, ok = f.sessions[""]
		}
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	// Catch-all so per-session non-GET ops resolve to a recordable
	// echo. Reverse-proxy tests check that requests land here when
	// routing local.
	mx.HandleFunc("/sessions/{id}/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&f.requests, 1)
		w.Header().Set("X-Local-Host", "true")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"echo":"local"}`))
	})
	f.srv = httptest.NewServer(mx)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeClankHost) setSession(id, infoJSON string) {
	f.mu.Lock()
	f.sessions[id] = infoJSON
	f.mu.Unlock()
}

func (f *fakeClankHost) setList(body string) {
	f.mu.Lock()
	f.list = body
	f.mu.Unlock()
}

// fakeRemoteGateway records every incoming request — used by tests
// that need to assert "the gateway forwarded to remote".
type fakeRemoteGateway struct {
	srv        *httptest.Server
	requests   int64
	authHeader string
	// worktrees is the body served on GET /v1/worktrees, so the
	// OwnerCache wired up in tests can pull ownership data.
	mu         sync.Mutex
	worktrees  string
	echoBody   string
}

func newFakeRemoteGateway(t *testing.T) *fakeRemoteGateway {
	t.Helper()
	f := &fakeRemoteGateway{}
	mx := http.NewServeMux()
	mx.HandleFunc("GET /v1/worktrees", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		body := f.worktrees
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mx.HandleFunc("/sessions/{id}/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&f.requests, 1)
		f.mu.Lock()
		f.authHeader = r.Header.Get("Authorization")
		body := f.echoBody
		f.mu.Unlock()
		w.Header().Set("X-Remote-Host", "true")
		w.WriteHeader(http.StatusOK)
		if body == "" {
			body = `{"echo":"remote"}`
		}
		_, _ = w.Write([]byte(body))
	})
	mx.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&f.requests, 1)
		f.mu.Lock()
		f.authHeader = r.Header.Get("Authorization")
		f.mu.Unlock()
		w.Header().Set("X-Remote-Host", "true")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"echo":"remote-create"}`))
	})
	f.srv = httptest.NewServer(mx)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRemoteGateway) setWorktrees(body string) {
	f.mu.Lock()
	f.worktrees = body
	f.mu.Unlock()
}

// buildLaptopGateway is the full per-test stack: provisioner pointing
// at fakeClankHost, OwnerCache pointing at fakeRemoteGateway.
func buildLaptopGateway(t *testing.T, host *fakeClankHost, remote *fakeRemoteGateway) http.Handler {
	t.Helper()
	prov := &stubProvisioner{ref: provisioner.HostRef{URL: host.srv.URL, Transport: http.DefaultTransport}}
	var resolver RemoteResolver
	var cache *OwnerCache
	if remote != nil {
		resolver = stubResolver{baseURL: remote.srv.URL, jwt: "test-jwt", ok: true}
		cache = NewOwnerCache(resolver, nil)
	}
	gw, err := NewGateway(Config{
		Provisioner:    prov,
		OwnerCache:     cache,
		RemoteResolver: resolver,
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return localAuth(gw.Handler(), "test-user")
}

func sessionInfoJSON(id, worktreeID string) string {
	return `{"id":"` + id + `","backend":"opencode","status":"idle","prompt":"hi","git_ref":{"worktree_id":"` + worktreeID + `"}}`
}

func TestSessionsRouter_NotMountedWithoutOwnerCache(t *testing.T) {
	t.Parallel()
	host := newFakeClankHost(t)
	// No remote → no OwnerCache → router stays unmounted; falls
	// through to today's catch-all proxyToHost.
	srv := httptest.NewServer(buildLaptopGateway(t, host, nil))
	t.Cleanup(srv.Close)

	host.setList(`[{"id":"s1","git_ref":{"worktree_id":"wt-A"}}]`)
	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "is_remote") {
		t.Errorf("decoration should not happen without an OwnerCache; got body=%s", body)
	}
}

func TestSessionsRouter_ListDecoration(t *testing.T) {
	t.Parallel()
	host := newFakeClankHost(t)
	remote := newFakeRemoteGateway(t)
	remote.setWorktrees(`{"worktrees":[{"id":"wt-A","owner_kind":"remote"},{"id":"wt-B","owner_kind":"local"}]}`)
	host.setList(`[
		{"id":"s1","git_ref":{"worktree_id":"wt-A"}},
		{"id":"s2","git_ref":{"worktree_id":"wt-B"}},
		{"id":"s3","git_ref":{}}
	]`)

	srv := httptest.NewServer(buildLaptopGateway(t, host, remote))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]bool{"s1": true, "s2": false, "s3": false}
	if len(rows) != 3 {
		t.Fatalf("rows=%d, want 3", len(rows))
	}
	for _, row := range rows {
		id, _ := row["id"].(string)
		isRemote, _ := row["is_remote"].(bool)
		if isRemote != want[id] {
			t.Errorf("session %s: is_remote=%v, want %v", id, isRemote, want[id])
		}
	}
}

func TestSessionsRouter_PerSession_LocalRouting(t *testing.T) {
	t.Parallel()
	host := newFakeClankHost(t)
	remote := newFakeRemoteGateway(t)
	remote.setWorktrees(`{"worktrees":[{"id":"wt-A","owner_kind":"local"}]}`)
	host.setSession("s1", sessionInfoJSON("s1", "wt-A"))

	srv := httptest.NewServer(buildLaptopGateway(t, host, remote))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/sessions/s1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Local-Host") != "true" {
		t.Errorf("local-owned worktree should route local; missing X-Local-Host. status=%d body=%s",
			resp.StatusCode, mustReadBody(resp))
	}
	if got := atomic.LoadInt64(&remote.requests); got != 0 {
		t.Errorf("remote should not have been called; got %d requests", got)
	}
}

func TestSessionsRouter_PerSession_RemoteRouting(t *testing.T) {
	t.Parallel()
	host := newFakeClankHost(t)
	remote := newFakeRemoteGateway(t)
	remote.setWorktrees(`{"worktrees":[{"id":"wt-A","owner_kind":"remote"}]}`)
	host.setSession("s1", sessionInfoJSON("s1", "wt-A"))

	srv := httptest.NewServer(buildLaptopGateway(t, host, remote))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/sessions/s1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Remote-Host") != "true" {
		t.Errorf("remote-owned worktree should route remote; missing X-Remote-Host. status=%d body=%s",
			resp.StatusCode, mustReadBody(resp))
	}
	if got := remote.authHeader; got != "Bearer test-jwt" {
		t.Errorf("Authorization on remote call = %q, want 'Bearer test-jwt'", got)
	}
}

func TestSessionsRouter_PerSession_404OnUnknownSession(t *testing.T) {
	t.Parallel()
	host := newFakeClankHost(t)
	remote := newFakeRemoteGateway(t)
	remote.setWorktrees(`{"worktrees":[]}`)
	// No session added → host returns 404.

	srv := httptest.NewServer(buildLaptopGateway(t, host, remote))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/sessions/missing/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}

func TestSessionsRouter_CreateLocal_NoHostname(t *testing.T) {
	t.Parallel()
	host := newFakeClankHost(t)
	remote := newFakeRemoteGateway(t)
	remote.setWorktrees(`{"worktrees":[]}`)

	srv := httptest.NewServer(buildLaptopGateway(t, host, remote))
	t.Cleanup(srv.Close)

	// fakeClankHost doesn't actually serve POST /sessions, so we
	// expect a 404 — but the key assertion is that the *remote*
	// was not called.
	resp, err := http.Post(srv.URL+"/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_ = mustReadBody(resp)
	if got := atomic.LoadInt64(&remote.requests); got != 0 {
		t.Errorf("remote was called %d times; want 0 (empty hostname → local)", got)
	}
}

func TestSessionsRouter_CreateRemote_HostnameSet(t *testing.T) {
	t.Parallel()
	host := newFakeClankHost(t)
	remote := newFakeRemoteGateway(t)

	srv := httptest.NewServer(buildLaptopGateway(t, host, remote))
	t.Cleanup(srv.Close)

	body := `{"hostname":"dev","backend":"opencode","prompt":"x","git_ref":{"local_path":"/tmp/x"}}`
	resp, err := http.Post(srv.URL+"/sessions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("X-Remote-Host") != "true" {
		t.Errorf("hostname=dev should route remote. status=%d body=%s", resp.StatusCode, mustReadBody(resp))
	}
}

func TestSessionsRouter_SearchDecoration(t *testing.T) {
	t.Parallel()
	host := newFakeClankHost(t)
	remote := newFakeRemoteGateway(t)
	remote.setWorktrees(`{"worktrees":[{"id":"wt-X","owner_kind":"remote"}]}`)
	host.setList(`[{"id":"s9","git_ref":{"worktree_id":"wt-X"}}]`)

	srv := httptest.NewServer(buildLaptopGateway(t, host, remote))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/sessions/search?q=foo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0]["is_remote"] != true {
		t.Errorf("search results: got %v, want one row with is_remote=true", rows)
	}
}

func mustReadBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// Static checks: confirm Config rejects misconfigurations early.
func TestNewGateway_OwnerCacheRequiresResolver(t *testing.T) {
	t.Parallel()
	resolver := stubResolver{ok: false}
	cache := NewOwnerCache(resolver, nil)
	_, err := NewGateway(Config{
		Provisioner: &stubProvisioner{},
		OwnerCache:  cache,
		// RemoteResolver intentionally nil.
	}, nil)
	if err == nil {
		t.Fatal("expected error when OwnerCache is set without RemoteResolver")
	}
}

func TestNewGateway_OwnerCacheRejectsCloudMode(t *testing.T) {
	t.Parallel()
	resolver := stubResolver{ok: false}
	cache := NewOwnerCache(resolver, nil)
	_, err := NewGateway(Config{
		Provisioner:    &stubProvisioner{},
		Sync:           &clanksync.Server{},
		OwnerCache:     cache,
		RemoteResolver: resolver,
	}, nil)
	if err == nil {
		t.Fatal("expected error when OwnerCache is set in cloud mode (Sync != nil)")
	}
}

// Compile-time check used by sessions_router.go.
var _ = auth.WithPrincipal
