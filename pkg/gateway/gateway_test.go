package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/provisioner"
)

// localAuth wraps next so each request gets a fixed test Principal.
// Mirrors what auth.Middleware + auth.AllowAll do in production but
// lets a test inject any UserID it wants.
func localAuth(next http.Handler, userID string) http.Handler {
	return auth.Middleware(next, &auth.AllowAll{UserID: userID})
}

// stubProvisioner is a minimal provisioner.Provisioner that returns
// a fixed HostRef. Tests configure the URL and Transport per case;
// ensureCalls lets a test pin "this code path didn't touch the
// provisioner" (e.g. /ping must answer locally).
type stubProvisioner struct {
	ref          provisioner.HostRef
	err          error
	ensureCalls  int
}

func (s *stubProvisioner) EnsureHost(context.Context, string) (provisioner.HostRef, error) {
	s.ensureCalls++
	return s.ref, s.err
}
func (*stubProvisioner) SuspendHost(context.Context, string) error { return nil }
func (*stubProvisioner) DestroyHost(context.Context, string) error { return nil }

// captureAuth records every Verify call so tests can assert the
// auth.Middleware invokes the configured Authenticator.
type captureAuth struct {
	calls   int
	allowed bool
}

func (c *captureAuth) Verify(*http.Request) (auth.Principal, error) {
	c.calls++
	if !c.allowed {
		return auth.Principal{}, errors.New("denied")
	}
	return auth.Principal{UserID: "tester"}, nil
}

func TestNewGateway_RequiresProvisioner(t *testing.T) {
	t.Parallel()
	if _, err := NewGateway(Config{}, nil); err == nil {
		t.Error("NewGateway with nil Provisioner returned nil error")
	}
}

func TestPing_DoesNotTouchProvisioner(t *testing.T) {
	t.Parallel()
	prov := &stubProvisioner{}
	g, err := NewGateway(Config{Provisioner: prov}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	srv := httptest.NewServer(localAuth(g.Handler(), "test"))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/ping status: got %d, want 200", resp.StatusCode)
	}
	// /ping must answer locally — without this assertion the test
	// would still pass even if /ping started waking the user's host.
	if prov.ensureCalls != 0 {
		t.Errorf("EnsureHost call count: got %d, want 0", prov.ensureCalls)
	}
}

func TestProxy_RejectsOnAuthFailure(t *testing.T) {
	t.Parallel()
	a := &captureAuth{allowed: false}
	prov := &stubProvisioner{}
	g, _ := NewGateway(Config{Provisioner: prov}, nil)
	srv := httptest.NewServer(auth.Middleware(g.Handler(), a))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatalf("GET /sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
	if a.calls != 1 {
		t.Errorf("auth.Verify call count: got %d, want 1", a.calls)
	}
}

func TestProxy_ForwardsToUpstream(t *testing.T) {
	t.Parallel()
	// Upstream that records the request it received.
	gotPath := make(chan string, 1)
	gotMethod := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		gotMethod <- r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)

	prov := &stubProvisioner{
		ref: provisioner.HostRef{
			URL:       upstream.URL,
			Transport: http.DefaultTransport,
			Hostname:  "test-host",
		},
	}
	g, _ := NewGateway(Config{Provisioner: prov}, nil)
	srv := httptest.NewServer(localAuth(g.Handler(), "test"))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/sessions/abc/messages")
	if err != nil {
		t.Fatalf("GET /sessions/abc/messages: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	if path := <-gotPath; path != "/sessions/abc/messages" {
		t.Errorf("upstream got path %q, want %q", path, "/sessions/abc/messages")
	}
	if m := <-gotMethod; m != http.MethodGet {
		t.Errorf("upstream got method %q, want GET", m)
	}
}

// TestProxy_BlocksSyncPathsWhenSyncNil locks in the laptop-gateway
// security boundary: when Sync is unconfigured (laptop mode), the
// gateway must refuse to forward /sync/* requests to its local
// clank-host subprocess. Otherwise any process with socket access
// could push a checkpoint into ~/work/ on the laptop.
func TestProxy_BlocksSyncPathsWhenSyncNil(t *testing.T) {
	t.Parallel()
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	prov := &stubProvisioner{
		ref: provisioner.HostRef{
			URL:       upstream.URL,
			Transport: http.DefaultTransport,
			Hostname:  "test-host",
		},
	}
	g, _ := NewGateway(Config{Provisioner: prov /* Sync intentionally nil */}, nil)
	srv := httptest.NewServer(localAuth(g.Handler(), "test"))
	t.Cleanup(srv.Close)

	// Both the direct path and the host-prefixed path must be blocked;
	// the gateway strips /hosts/<name>/ during proxying, so guarding only
	// the raw incoming path would let /hosts/local/sync/* bypass the
	// boundary and reach the host mux's unconditional /sync/* handlers.
	for _, path := range []string{"/sync/apply?repo=foo", "/hosts/local/sync/apply-from-urls", "/sync/build", "/sync/builds/abc/upload"} {
		upstreamCalled = false
		resp, err := http.Post(srv.URL+path, "application/octet-stream", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s status: got %d, want 404", path, resp.StatusCode)
		}
		resp.Body.Close()
		if upstreamCalled {
			t.Errorf("POST %s reached upstream; the gateway should have denied before proxying", path)
		}
	}
}

// TestProxy_AllowsSessionSyncOnLaptopGateway pins the carve-out:
// /sync/sessions/* IS allowed through on a laptop gateway, because
// those handlers don't touch ~/work/ — they drive opencode session
// export/import via the host Service. Without this, `clank push
// --migrate` can't reach the laptop's local clank-host for its
// session leg and the migration fails with a 404 from the proxy
// itself before any code runs.
func TestProxy_AllowsSessionSyncOnLaptopGateway(t *testing.T) {
	t.Parallel()
	var reached []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	prov := &stubProvisioner{ref: provisioner.HostRef{
		URL: upstream.URL, Transport: http.DefaultTransport, Hostname: "test-host",
	}}
	g, _ := NewGateway(Config{Provisioner: prov /* Sync intentionally nil */}, nil)
	srv := httptest.NewServer(localAuth(g.Handler(), "test"))
	t.Cleanup(srv.Close)

	for _, path := range []string{
		"/sync/sessions/build",
		"/sync/sessions/builds/abc/upload",
		"/sync/sessions/apply-from-urls",
		"/hosts/local/sync/sessions/build",
	} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("POST %s got 404 — the laptop guard is over-blocking the session-sync namespace", path)
		}
	}
	if len(reached) == 0 {
		t.Errorf("no session-sync request reached upstream; proxy is blocking the carve-out")
	}
}

func TestProxy_UsesHostRefTransport(t *testing.T) {
	t.Parallel()
	// Upstream that records the inbound Authorization header.
	gotAuth := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("X-Test-Header")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	// Custom transport that injects an X-Test-Header — simulates
	// what Daytona's preview-token injector or Sprites' bearer
	// injector do in production.
	tr := &headerInjector{wrapped: http.DefaultTransport, key: "X-Test-Header", value: "from-transport"}

	prov := &stubProvisioner{
		ref: provisioner.HostRef{URL: upstream.URL, Transport: tr},
	}
	g, _ := NewGateway(Config{Provisioner: prov}, nil)
	srv := httptest.NewServer(localAuth(g.Handler(), "test"))
	t.Cleanup(srv.Close)

	if _, err := http.Get(srv.URL + "/anything"); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got := <-gotAuth; got != "from-transport" {
		t.Errorf("upstream X-Test-Header: got %q, want from-transport", got)
	}
}

type headerInjector struct {
	wrapped    http.RoundTripper
	key, value string
}

func (h *headerInjector) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header.Set(h.key, h.value)
	return h.wrapped.RoundTrip(r2)
}

func TestProxy_SurfacesProvisionerFailure(t *testing.T) {
	t.Parallel()
	prov := &stubProvisioner{err: errSimulated}
	g, _ := NewGateway(Config{Provisioner: prov}, nil)
	srv := httptest.NewServer(localAuth(g.Handler(), "test"))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", resp.StatusCode)
	}
}

func TestProxy_SurfacesUpstreamFailure(t *testing.T) {
	t.Parallel()
	// HostRef points at a URL that won't accept connections.
	bogus, _ := url.Parse("http://127.0.0.1:1") // port 1: unlikely to listen
	_ = bogus
	prov := &stubProvisioner{
		ref: provisioner.HostRef{URL: "http://127.0.0.1:1", Transport: http.DefaultTransport},
	}
	g, _ := NewGateway(Config{Provisioner: prov}, nil)
	srv := httptest.NewServer(localAuth(g.Handler(), "test"))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", resp.StatusCode)
	}
}

// errSimulated is a sentinel for stubProvisioner.err.
var errSimulated = stubError("simulated")

type stubError string

func (e stubError) Error() string { return string(e) }

// TestProxy_StripsHostsPrefix pins the rewrite that turns
// /hosts/{name}/foo into /foo before forwarding to the host plane.
// The TUI's HostClient prepends /hosts/{hostname} to every host-scoped
// call (worktrees, auth) — this segment was a hub-era routing hint and
// the host's mux registers bare paths.
func TestProxy_StripsHostsPrefix(t *testing.T) {
	t.Parallel()
	gotPath := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	prov := &stubProvisioner{ref: provisioner.HostRef{URL: upstream.URL, Transport: http.DefaultTransport}}
	g, _ := NewGateway(Config{Provisioner: prov}, nil)
	srv := httptest.NewServer(localAuth(g.Handler(), "test"))
	t.Cleanup(srv.Close)

	cases := []struct {
		name   string
		in     string
		expect string
	}{
		{"auth-providers", "/hosts/local/auth/providers", "/auth/providers"},
		{"auth-apikey", "/hosts/local/auth/openai/apikey", "/auth/openai/apikey"},
		{"worktrees", "/hosts/local/worktrees/list-branches", "/worktrees/list-branches"},
		{"no-prefix-untouched", "/sessions", "/sessions"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + c.in)
			if err != nil {
				t.Fatalf("GET %s: %v", c.in, err)
			}
			resp.Body.Close()
			got := <-gotPath
			if got != c.expect {
				t.Errorf("upstream got %q, want %q (in: %q)", got, c.expect, c.in)
			}
		})
	}
}

// TestProxy_StripsHostsPrefixWithTargetPath pins that the strip works
// when the upstream URL itself has a path prefix — pre-fix the strip
// ran after SetURL had already joined target.Path with the incoming
// path, so /v1/hosts/{name}/auth would not match /hosts/ and the prefix
// would silently leak through.
func TestProxy_StripsHostsPrefixWithTargetPath(t *testing.T) {
	t.Parallel()
	gotPath := make(chan string, 1)
	mx := http.NewServeMux()
	mx.HandleFunc("/v1/auth/providers", func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	mx.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		w.WriteHeader(http.StatusNotFound)
	})
	upstream := httptest.NewServer(mx)
	t.Cleanup(upstream.Close)

	prov := &stubProvisioner{ref: provisioner.HostRef{URL: upstream.URL + "/v1", Transport: http.DefaultTransport}}
	g, _ := NewGateway(Config{Provisioner: prov}, nil)
	srv := httptest.NewServer(localAuth(g.Handler(), "test"))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/hosts/local/auth/providers")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	got := <-gotPath
	if got != "/v1/auth/providers" {
		t.Errorf("upstream got %q, want %q (target.Path=/v1 must be preserved + /hosts/local stripped)", got, "/v1/auth/providers")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200 (a 404 means the strip leaked /hosts/local through)", resp.StatusCode)
	}
}

func TestSingleJoiningSlash(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b, want string
	}{
		{"", "/foo", "/foo"},
		{"/v1", "", "/v1"},
		{"/v1", "/foo", "/v1/foo"},
		{"/v1/", "/foo", "/v1/foo"},
		{"/v1", "foo", "/v1/foo"},
		{"/v1/", "foo", "/v1/foo"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := singleJoiningSlash(c.a, c.b); got != c.want {
			t.Errorf("singleJoiningSlash(%q, %q) = %q; want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestStripHostsPrefix_Unit(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                              "",
		"/sessions":                     "/sessions",
		"/hosts/local":                  "/",
		"/hosts/local/":                 "/",
		"/hosts/local/auth/providers":   "/auth/providers",
		"/hosts/x-y-z/worktrees/resolve": "/worktrees/resolve",
		"/hostsfoo":                     "/hostsfoo", // doesn't start with /hosts/
	}
	for in, want := range cases {
		if got := stripHostsPrefix(in); got != want {
			t.Errorf("stripHostsPrefix(%q) = %q; want %q", in, got, want)
		}
	}
}
