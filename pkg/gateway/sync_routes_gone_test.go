package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/supaclank/clank/pkg/provisioner"
)

// TestCheckpointSyncRoutesGone pins the P6 deletion: the gateway no
// longer owns ANY checkpoint-sync route. GET /v1/worktrees (the old
// sync-DB listing), POST /v1/worktrees/create, the /sync legs, and the
// pull-back route are all unmounted — they fall through the specific
// /v1 routes to the plain host proxy, and a host without those routes
// (which is every host now) answers 404. The stub host here is an empty
// ServeMux, exactly mirroring a real clank-host that registers none of
// these paths.
//
// The per-route host-call assertion is the load-bearing half: before
// P6 these routes were answered gateway-side (by the embedded sync
// server or a gateway orchestration handler) without necessarily
// reaching the host at all. Reaching the host proves the gateway-side
// handler is truly gone, and the 404 proves nothing downstream serves
// it either.
func TestCheckpointSyncRoutesGone(t *testing.T) {
	t.Parallel()

	var hostHits int32
	// An empty mux: 404s every path, like a real host that no longer
	// registers /sync/* or a /v1 surface.
	emptyHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hostHits, 1)
		http.NotFound(w, r)
	}))
	t.Cleanup(emptyHost.Close)

	prov := &stubProvisioner{ref: provisioner.HostRef{URL: emptyHost.URL}}
	g, err := NewGateway(Config{Provisioner: prov}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(localAuth(g.Handler(), "tester"))
	t.Cleanup(gw.Close)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/worktrees"},
		{http.MethodPost, "/v1/worktrees/create"},
		{http.MethodPost, "/v1/worktrees/list-branches"},
		{http.MethodPost, "/v1/worktrees/sync"},
		{http.MethodPost, "/v1/worktrees/wt-1/sync"},
		{http.MethodPost, "/v1/worktrees/wt-1/pull"},
		{http.MethodPost, "/v1/checkpoints"},
	}
	for _, rt := range routes {
		before := atomic.LoadInt32(&hostHits)
		req, err := http.NewRequest(rt.method, gw.URL+rt.path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", rt.method, rt.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (route must be unmounted)", rt.method, rt.path, resp.StatusCode)
		}
		if after := atomic.LoadInt32(&hostHits); after != before+1 {
			t.Errorf("%s %s answered gateway-side (host hits %d→%d, want +1) — a checkpoint-sync handler survived", rt.method, rt.path, before, after)
		}
	}
}
