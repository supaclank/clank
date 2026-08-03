package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/supaclank/clank/pkg/provisioner"
)

const delUser = "tester"

// deletingSprite is an in-process stand-in for the clank-host's
// DELETE /worktrees/{id} endpoint. It counts calls and returns a
// configurable status so tests can pin the gateway's verbatim
// forwarding (the route is a pure host proxy).
type deletingSprite struct {
	status      int // response for DELETE /worktrees/{id}; 0 ⇒ 204
	body        string
	deleteCalls int32
	gotID       atomic.Value // string
}

func (ds *deletingSprite) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /worktrees/{id}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ds.deleteCalls, 1)
		ds.gotID.Store(r.PathValue("id"))
		if ds.status == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ds.status)
		_, _ = w.Write([]byte(ds.body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newDeleteGateway builds a gateway whose provisioner points at the
// given sprite. No gateway-side store: the host filesystem is the
// registry.
func newDeleteGateway(t *testing.T, sprite *httptest.Server) *httptest.Server {
	t.Helper()
	prov := &stubProvisioner{ref: provisioner.HostRef{URL: sprite.URL}}
	g, err := NewGateway(Config{Provisioner: prov}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := httptest.NewServer(localAuth(g.Handler(), delUser))
	t.Cleanup(gw.Close)
	return gw
}

func httpDelete(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestDeleteWorktree_HappyPath: the host's 204 forwards to the client;
// exactly one host call with the requested id.
func TestDeleteWorktree_HappyPath(t *testing.T) {
	t.Parallel()
	sprite := &deletingSprite{status: http.StatusNoContent}
	gw := newDeleteGateway(t, sprite.server(t))

	resp := httpDelete(t, gw.URL+"/v1/worktrees/wt-del")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&sprite.deleteCalls); n != 1 {
		t.Fatalf("sprite delete calls=%d, want 1", n)
	}
	if got := sprite.gotID.Load(); got != "wt-del" {
		t.Fatalf("sprite got id=%v, want wt-del", got)
	}
}

// TestDeleteWorktree_BusyForwards409: the host's 409 (active session)
// reaches the client verbatim, typed body included.
func TestDeleteWorktree_BusyForwards409(t *testing.T) {
	t.Parallel()
	sprite := &deletingSprite{status: http.StatusConflict, body: `{"code":"worktree_busy"}`}
	gw := newDeleteGateway(t, sprite.server(t))

	resp := httpDelete(t, gw.URL+"/v1/worktrees/wt-busy")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409 for a busy worktree", resp.StatusCode)
	}
}

// TestDeleteWorktree_HostErrorForwardsVerbatim: a host 500 forwards
// untouched — the route is a pure proxy, no gateway-side masking and no
// gateway-side state to protect.
func TestDeleteWorktree_HostErrorForwardsVerbatim(t *testing.T) {
	t.Parallel()
	sprite := &deletingSprite{status: http.StatusInternalServerError, body: `{"code":"internal"}`}
	gw := newDeleteGateway(t, sprite.server(t))

	resp := httpDelete(t, gw.URL+"/v1/worktrees/wt-err")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 forwarded verbatim", resp.StatusCode)
	}
}
