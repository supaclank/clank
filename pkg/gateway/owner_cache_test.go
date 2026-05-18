package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clanksync "github.com/acksell/clank/pkg/sync"
)

type stubResolver struct {
	baseURL, jwt string
	ok           bool
}

func (r stubResolver) ActiveRemote() (string, string, bool) {
	return r.baseURL, r.jwt, r.ok
}

// fakeRemote spins up an httptest server serving GET /v1/worktrees
// with the given snapshot. The atomic counter records how many times
// the endpoint was hit so tests can assert refresh/dedupe semantics.
type fakeRemote struct {
	srv       *httptest.Server
	calls     int64
	mu        sync.Mutex
	snapshot  []map[string]any
	expectJWT string // when non-empty, request without matching bearer returns 401
}

func newFakeRemote(t *testing.T) *fakeRemote {
	t.Helper()
	f := &fakeRemote{}
	mx := http.NewServeMux()
	mx.HandleFunc("GET /v1/worktrees", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&f.calls, 1)
		if f.expectJWT != "" && r.Header.Get("Authorization") != "Bearer "+f.expectJWT {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		snap := f.snapshot
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"worktrees": snap})
	})
	f.srv = httptest.NewServer(mx)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRemote) setSnapshot(wts []map[string]any) {
	f.mu.Lock()
	f.snapshot = wts
	f.mu.Unlock()
}

func wt(id string, kind clanksync.OwnerKind) map[string]any {
	return map[string]any{"id": id, "owner_kind": string(kind)}
}

func TestOwnerCache_ColdLookupRefreshes(t *testing.T) {
	t.Parallel()
	f := newFakeRemote(t)
	f.setSnapshot([]map[string]any{
		wt("wt-A", clanksync.OwnerKindLocal),
		wt("wt-B", clanksync.OwnerKindRemote),
	})
	c := NewOwnerCache(stubResolver{baseURL: f.srv.URL, ok: true}, nil)

	kind, ok, err := c.Lookup(context.Background(), "wt-B")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !ok || kind != clanksync.OwnerKindRemote {
		t.Errorf("lookup wt-B: got (%q, %v); want (remote, true)", kind, ok)
	}
	if atomic.LoadInt64(&f.calls) != 1 {
		t.Errorf("calls: got %d, want 1", atomic.LoadInt64(&f.calls))
	}
}

func TestOwnerCache_UnknownWorktree(t *testing.T) {
	t.Parallel()
	f := newFakeRemote(t)
	f.setSnapshot([]map[string]any{wt("wt-A", clanksync.OwnerKindLocal)})
	c := NewOwnerCache(stubResolver{baseURL: f.srv.URL, ok: true}, nil)

	_, ok, err := c.Lookup(context.Background(), "wt-missing")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ok {
		t.Errorf("unknown wt should return ok=false")
	}
}

func TestOwnerCache_ServesCacheWithinTTL(t *testing.T) {
	t.Parallel()
	f := newFakeRemote(t)
	f.setSnapshot([]map[string]any{wt("wt-A", clanksync.OwnerKindRemote)})
	c := NewOwnerCache(stubResolver{baseURL: f.srv.URL, ok: true}, nil)

	// Prime.
	if _, _, err := c.Lookup(context.Background(), "wt-A"); err != nil {
		t.Fatal(err)
	}
	// Two more lookups within TTL: no additional calls.
	for i := 0; i < 5; i++ {
		if _, _, err := c.Lookup(context.Background(), "wt-A"); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt64(&f.calls); got != 1 {
		t.Errorf("calls: got %d, want 1 (TTL should suppress refresh)", got)
	}
}

func TestOwnerCache_TTLExpiryRefreshes(t *testing.T) {
	t.Parallel()
	f := newFakeRemote(t)
	f.setSnapshot([]map[string]any{wt("wt-A", clanksync.OwnerKindLocal)})
	c := NewOwnerCache(stubResolver{baseURL: f.srv.URL, ok: true}, nil)
	c.SetTTL(10 * time.Millisecond)

	if _, _, err := c.Lookup(context.Background(), "wt-A"); err != nil {
		t.Fatal(err)
	}

	// Server's view of the worktree flips remote-owned.
	f.setSnapshot([]map[string]any{wt("wt-A", clanksync.OwnerKindRemote)})

	// Wait for TTL.
	time.Sleep(30 * time.Millisecond)

	kind, ok, err := c.Lookup(context.Background(), "wt-A")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || kind != clanksync.OwnerKindRemote {
		t.Errorf("post-TTL: got (%q, %v); want (remote, true)", kind, ok)
	}
	if got := atomic.LoadInt64(&f.calls); got != 2 {
		t.Errorf("calls: got %d, want 2 (one prime + one TTL-driven refresh)", got)
	}
}

func TestOwnerCache_InvalidateForcesRefresh(t *testing.T) {
	t.Parallel()
	f := newFakeRemote(t)
	f.setSnapshot([]map[string]any{wt("wt-A", clanksync.OwnerKindLocal)})
	c := NewOwnerCache(stubResolver{baseURL: f.srv.URL, ok: true}, nil)

	if _, _, err := c.Lookup(context.Background(), "wt-A"); err != nil {
		t.Fatal(err)
	}
	c.Invalidate()
	if _, _, err := c.Lookup(context.Background(), "wt-A"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&f.calls); got != 2 {
		t.Errorf("calls: got %d, want 2", got)
	}
}

func TestOwnerCache_ConcurrentLookupsDedupe(t *testing.T) {
	t.Parallel()
	f := newFakeRemote(t)
	// Slow handler so concurrent lookups overlap. The singleflight
	// guarantee is that they collapse to one call regardless.
	mx := http.NewServeMux()
	mx.HandleFunc("GET /v1/worktrees", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&f.calls, 1)
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"worktrees": []map[string]any{wt("wt-A", clanksync.OwnerKindRemote)},
		})
	})
	srv := httptest.NewServer(mx)
	t.Cleanup(srv.Close)

	c := NewOwnerCache(stubResolver{baseURL: srv.URL, ok: true}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = c.Lookup(context.Background(), "wt-A")
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&f.calls); got != 1 {
		t.Errorf("calls: got %d, want 1 (singleflight should dedupe)", got)
	}
}

func TestOwnerCache_NoActiveRemote(t *testing.T) {
	t.Parallel()
	f := newFakeRemote(t)
	c := NewOwnerCache(stubResolver{ok: false}, nil)

	_, ok, err := c.Lookup(context.Background(), "wt-A")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ok {
		t.Errorf("no active remote should return ok=false")
	}
	if got := atomic.LoadInt64(&f.calls); got != 0 {
		t.Errorf("calls: got %d, want 0 (no remote should not hit network)", got)
	}
}

func TestOwnerCache_RemoteUnreachable_NoPriorData(t *testing.T) {
	t.Parallel()
	c := NewOwnerCache(stubResolver{baseURL: "http://127.0.0.1:1", ok: true}, // port 1: unlikely to listen
		&http.Client{Timeout: 100 * time.Millisecond})

	_, _, err := c.Lookup(context.Background(), "wt-A")
	if err == nil {
		t.Fatal("expected error when remote unreachable with no prior data")
	}
}

func TestOwnerCache_RemoteUnreachable_FallsBackOnPriorData(t *testing.T) {
	t.Parallel()
	f := newFakeRemote(t)
	f.setSnapshot([]map[string]any{wt("wt-A", clanksync.OwnerKindRemote)})
	c := NewOwnerCache(stubResolver{baseURL: f.srv.URL, ok: true}, nil)
	c.SetTTL(10 * time.Millisecond)

	if _, _, err := c.Lookup(context.Background(), "wt-A"); err != nil {
		t.Fatal(err)
	}
	// Now point resolver at a dead address; cache has prior entries.
	c.resolver = stubResolver{baseURL: "http://127.0.0.1:1", ok: true}
	c.httpClient = &http.Client{Timeout: 100 * time.Millisecond}
	time.Sleep(20 * time.Millisecond)

	kind, ok, err := c.Lookup(context.Background(), "wt-A")
	if err != nil {
		t.Errorf("lookup with prior data should succeed despite refresh failure; got %v", err)
	}
	if !ok || kind != clanksync.OwnerKindRemote {
		t.Errorf("fallback: got (%q, %v); want (remote, true)", kind, ok)
	}
}

func TestOwnerCache_SendsBearerToken(t *testing.T) {
	t.Parallel()
	f := newFakeRemote(t)
	f.expectJWT = "test-jwt-abc"
	f.setSnapshot([]map[string]any{wt("wt-A", clanksync.OwnerKindRemote)})
	c := NewOwnerCache(stubResolver{baseURL: f.srv.URL, jwt: "test-jwt-abc", ok: true}, nil)

	_, _, err := c.Lookup(context.Background(), "wt-A")
	if err != nil {
		t.Fatalf("lookup with correct JWT: %v", err)
	}

	// Wrong JWT → fake remote returns 401 → refresh fails → no prior
	// data so err propagates.
	c2 := NewOwnerCache(stubResolver{baseURL: f.srv.URL, jwt: "wrong", ok: true}, nil)
	if _, _, err := c2.Lookup(context.Background(), "wt-A"); err == nil {
		t.Errorf("wrong JWT should fail refresh")
	}
}

func TestOwnerCache_EmptyWorktreeID(t *testing.T) {
	t.Parallel()
	c := NewOwnerCache(stubResolver{ok: false}, nil)
	if _, _, err := c.Lookup(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty worktree_id")
	}
}
