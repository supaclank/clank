package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/provisioner"
)

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
// gateway invokes auth.
type captureAuth struct {
	calls   int
	allowed bool
}

func (c *captureAuth) Verify(*http.Request) (map[string]any, error) {
	c.calls++
	if !c.allowed {
		return nil, http.ErrNoCookie // any non-nil error
	}
	return map[string]any{"sub": "tester"}, nil
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
	srv := httptest.NewServer(g.Handler())
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
	auth := &captureAuth{allowed: false}
	prov := &stubProvisioner{}
	g, _ := NewGateway(Config{Provisioner: prov, Auth: auth}, nil)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatalf("GET /sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate: got %q, want to contain Bearer", got)
	}
	if auth.calls != 1 {
		t.Errorf("auth.Verify call count: got %d, want 1", auth.calls)
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
			Hostname:  host.Hostname("test-host"),
		},
	}
	g, _ := NewGateway(Config{Provisioner: prov}, nil)
	srv := httptest.NewServer(g.Handler())
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
	srv := httptest.NewServer(g.Handler())
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
	srv := httptest.NewServer(g.Handler())
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
	srv := httptest.NewServer(g.Handler())
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
	srv := httptest.NewServer(g.Handler())
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
	srv := httptest.NewServer(g.Handler())
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
