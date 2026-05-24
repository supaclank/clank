package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acksell/clank/pkg/provisioner"
)

// captureHost is a tiny fake host: every request it sees is
// recorded so the gateway test can assert path/method/body/query
// were forwarded faithfully. status and body are scriptable per
// test.
type captureHost struct {
	mu     atomic.Bool
	method atomic.Value // string
	path   atomic.Value // string
	query  atomic.Value // string
	body   atomic.Value // []byte

	respStatus int
	respCT     string
	respBody   string
}

func (h *captureHost) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Store(true)
		h.method.Store(r.Method)
		h.path.Store(r.URL.Path)
		h.query.Store(r.URL.RawQuery)
		body, _ := io.ReadAll(r.Body)
		h.body.Store(body)
		if h.respCT != "" {
			w.Header().Set("Content-Type", h.respCT)
		}
		w.WriteHeader(h.respStatus)
		_, _ = w.Write([]byte(h.respBody))
	})
}

func newGatewayForGitHubProxyTest(t *testing.T, host *captureHost) http.Handler {
	t.Helper()
	srv := httptest.NewServer(host.handler())
	t.Cleanup(srv.Close)

	g, err := NewGateway(Config{
		Provisioner: &stubProvisioner{
			ref: provisioner.HostRef{URL: srv.URL},
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return localAuth(g.Handler(), "test-user")
}

func TestGitHubProxy_Status_ForwardsToHost(t *testing.T) {
	t.Parallel()
	host := &captureHost{
		respStatus: http.StatusOK,
		respCT:     "application/json",
		respBody:   `{"available":true,"connected":false}`,
	}
	gw := newGatewayForGitHubProxyTest(t, host)

	req := httptest.NewRequest(http.MethodGet, "/v1/github/status", nil)
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != `{"available":true,"connected":false}` {
		t.Errorf("body forwarded verbatim: got %q", got)
	}
	if host.path.Load() != "/credentials/github/status" {
		t.Errorf("host path = %v", host.path.Load())
	}
	if host.method.Load() != http.MethodGet {
		t.Errorf("host method = %v", host.method.Load())
	}
}

func TestGitHubProxy_Disconnect_ForwardsDelete(t *testing.T) {
	t.Parallel()
	host := &captureHost{respStatus: http.StatusNoContent}
	gw := newGatewayForGitHubProxyTest(t, host)

	req := httptest.NewRequest(http.MethodDelete, "/v1/github", nil)
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rr.Code)
	}
	if host.path.Load() != "/credentials/github" {
		t.Errorf("host path = %v", host.path.Load())
	}
	if host.method.Load() != http.MethodDelete {
		t.Errorf("host method = %v", host.method.Load())
	}
}

func TestGitHubProxy_ConnectStart_ForwardsBody(t *testing.T) {
	t.Parallel()
	host := &captureHost{
		respStatus: http.StatusOK,
		respCT:     "application/json",
		respBody:   `{"flow_id":"flow-1","user_code":"WXYZ-7890"}`,
	}
	gw := newGatewayForGitHubProxyTest(t, host)

	req := httptest.NewRequest(http.MethodPost, "/v1/github/connect/start", bytes.NewReader(nil))
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "flow-1") {
		t.Errorf("body not forwarded: %s", rr.Body.String())
	}
	if host.path.Load() != "/credentials/github/connect/start" {
		t.Errorf("host path = %v", host.path.Load())
	}
}

func TestGitHubProxy_ConnectStatus_ForwardsQuery(t *testing.T) {
	t.Parallel()
	host := &captureHost{
		respStatus: http.StatusOK,
		respCT:     "application/json",
		respBody:   `{"state":"pending"}`,
	}
	gw := newGatewayForGitHubProxyTest(t, host)

	req := httptest.NewRequest(http.MethodGet, "/v1/github/connect/status?flow_id=abc", nil)
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)

	if host.path.Load() != "/credentials/github/connect/status" {
		t.Errorf("host path = %v", host.path.Load())
	}
	if host.query.Load() != "flow_id=abc" {
		t.Errorf("host query = %v", host.query.Load())
	}
}

func TestGitHubProxy_PreviewPR_ForwardsID(t *testing.T) {
	t.Parallel()
	host := &captureHost{
		respStatus: http.StatusOK,
		respCT:     "application/json",
		respBody:   `{"origin_state":"github","owner":"acme","repo":"api","head_branch":"feat","head_sha":"abc1234"}`,
	}
	gw := newGatewayForGitHubProxyTest(t, host)

	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/01WT/pr/preview", nil)
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if host.path.Load() != "/worktrees/01WT/pr/preview" {
		t.Errorf("host path = %v, want /worktrees/01WT/pr/preview", host.path.Load())
	}
	if !strings.Contains(rr.Body.String(), `"owner":"acme"`) {
		t.Errorf("body not forwarded verbatim: %s", rr.Body.String())
	}
}

func TestGitHubProxy_CreatePR_ForwardsIDAndBody(t *testing.T) {
	t.Parallel()
	host := &captureHost{
		respStatus: http.StatusCreated,
		respCT:     "application/json",
		respBody:   `{"pr_number":42,"pr_url":"https://github.com/x/y/pull/42"}`,
	}
	gw := newGatewayForGitHubProxyTest(t, host)

	body := `{"title":"feat","body":"body","base":"main","draft":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/worktrees/01HOST/pr", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if host.path.Load() != "/worktrees/01HOST/pr" {
		t.Errorf("host path = %v, want /worktrees/01HOST/pr", host.path.Load())
	}
	if got := host.body.Load().([]byte); string(got) != body {
		t.Errorf("host body not forwarded verbatim: got %q want %q", got, body)
	}
}

func TestGitHubProxy_RequiresAuth(t *testing.T) {
	t.Parallel()
	host := &captureHost{respStatus: http.StatusOK}
	srv := httptest.NewServer(host.handler())
	t.Cleanup(srv.Close)

	g, err := NewGateway(Config{
		Provisioner: &stubProvisioner{ref: provisioner.HostRef{URL: srv.URL}},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	// Wrap with a Verify-fails auth so any request 401s.
	gw := localAuth(g.Handler(), "")

	req := httptest.NewRequest(http.MethodGet, "/v1/github/status", nil)
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	// Host should never have been hit when auth fails.
	if host.mu.Load() {
		t.Errorf("host received a request despite auth failure")
	}
}

func TestGitHubProxy_ProvisionerFailure_502(t *testing.T) {
	t.Parallel()
	g, err := NewGateway(Config{
		Provisioner: &stubProvisioner{err: io.EOF},
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw := localAuth(g.Handler(), "tester")

	req := httptest.NewRequest(http.MethodGet, "/v1/github/status", nil)
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
}
