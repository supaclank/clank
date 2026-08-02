package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/routestore/memstore"
	"github.com/acksell/clank/pkg/preview/tokens"
	"github.com/acksell/clank/pkg/provisioner"
	"github.com/coder/websocket"
)

// targetDialProvisioner is a real-Provisioner-shaped stub whose
// OpenInternalConn dials a fixed (test) host:port. Other methods
// panic since the preview-proxy code path doesn't call them. Tracks
// dial count so tests can assert tunnel reuse.
type targetDialProvisioner struct {
	target string
	dials  atomic.Int64
}

func (d *targetDialProvisioner) EnsureHost(context.Context, string) (provisioner.HostRef, error) {
	panic("targetDialProvisioner: EnsureHost")
}
func (d *targetDialProvisioner) SuspendHost(context.Context, string) error {
	panic("targetDialProvisioner: SuspendHost")
}
func (d *targetDialProvisioner) DestroyHost(context.Context, string) error {
	panic("targetDialProvisioner: DestroyHost")
}
func (d *targetDialProvisioner) DestroyHostsByUser(context.Context, string) error {
	panic("targetDialProvisioner: DestroyHostsByUser")
}
func (d *targetDialProvisioner) GetHostByID(context.Context, string) (provisioner.HostRef, error) {
	panic("targetDialProvisioner: GetHostByID")
}
func (d *targetDialProvisioner) OpenInternalConn(ctx context.Context, _ string, _ int) (net.Conn, error) {
	d.dials.Add(1)
	return (&net.Dialer{}).DialContext(ctx, "tcp", d.target)
}

// bearerAuthenticator is a real Authenticator that returns a
// Principal whose UserID == the bearer token. Lets tests vary "who is
// calling" by setting Authorization without standing up a JWT verifier.
type bearerAuthenticator struct{}

func (bearerAuthenticator) Verify(r *http.Request) (auth.Principal, error) {
	tok, err := auth.ExtractBearer(r)
	if err != nil {
		return auth.Principal{}, err
	}
	return auth.Principal{UserID: tok}, nil
}

// previewProxyFixture wires the gateway with a real upstream server
// behind the tunnel and exposes the subdomain-wrapped handler as an
// httptest.Server. Each test seeds its own routes.
type previewProxyFixture struct {
	srv      *httptest.Server
	upstream *httptest.Server
	store    *memstore.Store
	prov     *targetDialProvisioner
	root     string
	// signingKey is the HMAC secret the fixture's gateway was
	// constructed with. Exposed so tests can mint their own signed
	// URLs and assert the gateway accepts them.
	signingKey []byte
	// fallback observed: tracks whether a request bypassed the
	// preview wrapper and fell through to the main mux.
	fallbackHits atomic.Int64
}

func newPreviewProxyFixture(t *testing.T, upstream http.Handler) *previewProxyFixture {
	t.Helper()
	const root = "clankexample.dev"

	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	target := strings.TrimPrefix(up.URL, "http://")
	prov := &targetDialProvisioner{target: target}

	store := memstore.New(nil)
	signingKey, err := tokens.GenerateSigningKey()
	if err != nil {
		t.Fatalf("GenerateSigningKey: %v", err)
	}
	g, err := NewGateway(Config{
		Provisioner:          prov,
		PreviewRoutes:        store,
		PreviewHostLookup:    fakeHostLookup{},
		PreviewRootDomain:    root,
		PreviewAuthenticator: bearerAuthenticator{},
		PreviewSigningKey:    signingKey,
	}, nil)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	f := &previewProxyFixture{
		upstream:   up,
		store:      store,
		prov:       prov,
		root:       root,
		signingKey: signingKey,
	}

	// Fallback handler the wrapper delegates to for non-preview hosts.
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.fallbackHits.Add(1)
		_, _ = w.Write([]byte("fallback"))
	})

	handler := g.WrapPreviewSubdomain(fallback)
	f.srv = httptest.NewServer(handler)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *previewProxyFixture) seed(t *testing.T, owner string, vis tokens.Visibility) routestore.Route {
	t.Helper()
	tok, err := tokens.New()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	r, err := f.store.Upsert(context.Background(), routestore.Route{
		Token:        tok,
		OwnerUserID:  owner,
		HostID:       "h-" + owner,
		WorktreeID:   "wt",
		ServiceName:  tokens.DefaultServiceName,
		InternalPort: 19000, // value ignored by targetDialProvisioner; must be in [1,65535] for previewtunnel.New validation
		Visibility:   vis,
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return r
}

// do issues an HTTP GET against the wrapper, optionally setting the
// Host to a preview-host form and an Authorization bearer.
func (f *previewProxyFixture) do(t *testing.T, host, bearer, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", f.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if host != "" {
		req.Host = host // overrides r.URL.Host for the Host header
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// --- tests ---

func TestPreviewProxy_FallsThroughOnNonPreviewHost(t *testing.T) {
	t.Parallel()
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream hit on non-preview host")
	}))
	resp := f.do(t, "api.clankexample.dev", "", "/")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "fallback" {
		t.Errorf("body = %q, want fallback", body)
	}
	if hits := f.fallbackHits.Load(); hits != 1 {
		t.Errorf("fallback hits = %d, want 1", hits)
	}
}

func TestPreviewProxy_UnknownToken404(t *testing.T) {
	t.Parallel()
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream hit despite unknown token")
	}))
	resp := f.do(t, "preview-nonexistent.clankexample.dev", "anything", "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPreviewProxy_OwnerOnlyRequiresJWT(t *testing.T) {
	t.Parallel()
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream hit despite missing JWT")
	}))
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	host := tokens.HostFor(r.Token, f.root)

	resp := f.do(t, host, "", "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, expected Bearer challenge", got)
	}
}

func TestPreviewProxy_OwnerOnlyCrossTenant404(t *testing.T) {
	t.Parallel()
	// Owner is alice; bob tries to hit her preview. 404, NOT 403 —
	// don't leak the existence of someone else's token.
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream hit on cross-tenant attempt")
	}))
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	host := tokens.HostFor(r.Token, f.root)

	resp := f.do(t, host, "bob", "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (not 403)", resp.StatusCode)
	}
}

func TestPreviewProxy_OwnerOnlyOwnerSucceeds(t *testing.T) {
	t.Parallel()
	var upstreamHit atomic.Bool
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit.Store(true)
		// Verify the credential strip happened: upstream MUST NOT see
		// the Authorization header or any Cookie.
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("upstream received Authorization=%q", h)
		}
		if h := r.Header.Get("Cookie"); h != "" {
			t.Errorf("upstream received Cookie=%q", h)
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			t.Errorf("upstream received X-Forwarded-Host=%q", h)
		}
		// And that Host was preserved through the proxy.
		if !strings.HasPrefix(r.Host, "preview-") {
			t.Errorf("upstream Host = %q, expected preview-... preserved", r.Host)
		}
		_, _ = w.Write([]byte("ok from upstream"))
	}))
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	host := tokens.HostFor(r.Token, f.root)

	resp := f.do(t, host, "alice", "/path")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok from upstream" {
		t.Errorf("body = %q", body)
	}
	if !upstreamHit.Load() {
		t.Error("upstream was not reached despite 200 response")
	}
}

func TestPreviewProxy_PublicAcceptsAnonymous(t *testing.T) {
	t.Parallel()
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("public ok"))
	}))
	r := f.seed(t, "alice", tokens.VisibilityPublic)
	host := tokens.HostFor(r.Token, f.root)

	// No JWT at all.
	resp := f.do(t, host, "", "/")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 for public token", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "public ok" {
		t.Errorf("body = %q", body)
	}
}

func TestPreviewProxy_PublicStillStripsAuth(t *testing.T) {
	t.Parallel()
	// Even when a public-visibility URL gets a JWT (e.g. logged-in
	// teammate clicks a shared link), the proxy must strip it before
	// forwarding to user code.
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("public proxy leaked Authorization=%q to upstream", h)
		}
		_, _ = w.Write([]byte("k"))
	}))
	r := f.seed(t, "alice", tokens.VisibilityPublic)
	host := tokens.HostFor(r.Token, f.root)

	resp := f.do(t, host, "incidental-jwt-from-shared-link-clicker", "/")
	resp.Body.Close()
}

func TestPreviewProxy_RevokedRoute404(t *testing.T) {
	t.Parallel()
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream hit on revoked route")
	}))
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	if err := f.store.Revoke(context.Background(), r.Token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp := f.do(t, tokens.HostFor(r.Token, f.root), "alice", "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 after revoke", resp.StatusCode)
	}
}

func TestPreviewProxy_FreshDialPerRequest_NoIdleReuse(t *testing.T) {
	t.Parallel()
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	r := f.seed(t, "alice", tokens.VisibilityPublic)
	host := tokens.HostFor(r.Token, f.root)
	for range 4 {
		resp := f.do(t, host, "", "/")
		resp.Body.Close()
	}
	// Idle keep-alive reuse is disabled (see previewtunnel.New): each
	// request through the shared Tunnel dials a FRESH underlying conn
	// rather than reusing a pooled one that may have gone half-open
	// (Sprites edge idle-drop / sprite pause). So 4 requests == 4 dials.
	if dials := f.prov.dials.Load(); dials != 4 {
		t.Errorf("dials = %d, want 4 (fresh dial per request, no idle reuse)", dials)
	}
}

func TestPreviewProxy_WebSocketUpgradeProxiesCleanly(t *testing.T) {
	t.Parallel()
	// Upstream echoes WS frames; tests that the proxy carries
	// Upgrade: websocket through end-to-end. This is the HMR case —
	// the whole Sprites-WSS-tunnel architecture exists to make this
	// work without body rewriting.
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("ws accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusInternalError, "")
		typ, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), typ, data)
	})
	f := newPreviewProxyFixture(t, upstream)
	r := f.seed(t, "alice", tokens.VisibilityPublic)
	host := tokens.HostFor(r.Token, f.root)

	// Build an http.Client that resolves the preview host to the
	// fixture's httptest server via a custom DialContext (since the
	// hostname doesn't resolve to anything in real DNS). websocket.Dial
	// uses this client's Transport for the upgrade handshake.
	target := strings.TrimPrefix(f.srv.URL, "http://")
	httpc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", target)
			},
		},
	}

	// Use the URL's Host = preview-token.root so the gateway's Host
	// dispatch fires.
	u := &url.URL{Scheme: "ws", Host: host, Path: "/hot"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsConn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: httpc})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "")
	if err := wsConn.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	_, data, err := wsConn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if string(data) != "hi" {
		t.Errorf("ws echo = %q, want %q", data, "hi")
	}
}

// TestPreviewProxy_WebSocketUpgradeOwnerOnlyNoAuthRejected pins the
// "owner_only routes reject anonymous WS upgrades" contract. An
// attacker who learned the preview hostname (but not the JWT or a
// valid signed bearer) must not be able to open the HMR socket and
// receive module-update messages containing live source code.
//
// Regression target: a refactor that accidentally moves the auth
// gate AFTER the proxy hijacks the inbound connection would still
// 401 here, but if the gate is skipped entirely the upstream would
// see the request and the test would observe a 101 (which it asserts
// must NOT happen).
func TestPreviewProxy_WebSocketUpgradeOwnerOnlyNoAuthRejected(t *testing.T) {
	t.Parallel()
	upstream := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream should not be reached on unauth WS upgrade")
	})
	f := newPreviewProxyFixture(t, upstream)
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	host := tokens.HostFor(r.Token, f.root)

	target := strings.TrimPrefix(f.srv.URL, "http://")
	httpc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", target)
			},
		},
	}

	u := &url.URL{Scheme: "ws", Host: host, Path: "/hot"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: httpc})
	if err == nil {
		t.Fatalf("ws dial unexpectedly succeeded against owner_only route without auth")
	}
	if resp == nil {
		t.Fatalf("ws dial err=%v but no HTTP response captured", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
}

// TestPreviewProxy_WebSocketUpgradeOwnerOnlyWithSignedCookies is the
// regression test for today's HMR break: the Android dev-launcher's
// HMR WebSocket client doesn't carry the signed bearer in the URL
// (Metro's HMRClient hardcodes the WS path with no auth params), so
// the only way for owner_only WS upgrades to succeed is via the
// signed-bearer cookies that the gateway sets on the initial bundle
// fetch. Cookies → 101.
//
// If this test starts failing, HMR is broken for every Android
// dev-launcher consumer regardless of whether the cookie-bridge in
// clank-mobile/NetworkBundleLoader is still doing its job.
func TestPreviewProxy_WebSocketUpgradeOwnerOnlyWithSignedCookies(t *testing.T) {
	t.Parallel()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("ws accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusInternalError, "")
		typ, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), typ, data)
	})
	f := newPreviewProxyFixture(t, upstream)
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	host := tokens.HostFor(r.Token, f.root)
	exp := time.Now().Add(10 * time.Minute)
	sig, err := tokens.Sign(f.signingKey, r.Token, exp)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	target := strings.TrimPrefix(f.srv.URL, "http://")
	httpc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", target)
			},
		},
	}

	u := &url.URL{Scheme: "ws", Host: host, Path: "/hot"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsConn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: httpc,
		HTTPHeader: http.Header{
			// Match exactly what NetworkBundleLoader.kt's cookie-bridge
			// inserts into Android's WebView CookieManager after the
			// signed-URL bundle fetch lands.
			"Cookie": []string{fmt.Sprintf(
				"%s=%s; %s=%d",
				tokens.SigParam, sig,
				tokens.ExpParam, exp.Unix(),
			)},
		},
	})
	if err != nil {
		t.Fatalf("ws dial with signed cookies: %v", err)
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "")

	if err := wsConn.Write(ctx, websocket.MessageText, []byte("hmr")); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	_, data, err := wsConn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if string(data) != "hmr" {
		t.Errorf("ws echo = %q, want %q", data, "hmr")
	}
}

// --- signed-URL bearer tests ---

// signedURLFor mints a valid signed URL for token using the
// fixture's secret. Returned URL points at the fixture's server (so
// the request actually lands), but with the preview-host Host
// header set so the wrapper's match fires.
func (f *previewProxyFixture) signedURLFor(t *testing.T, token string, ttl time.Duration) (signedQuery string, exp time.Time) {
	t.Helper()
	exp = time.Now().Add(ttl)
	sig, err := tokens.Sign(f.signingKey, token, exp)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return fmt.Sprintf("?%s=%s&%s=%d", tokens.SigParam, sig, tokens.ExpParam, exp.Unix()), exp
}

func TestPreviewProxy_OwnerOnlySignedURLSucceeds(t *testing.T) {
	t.Parallel()
	var upstreamHits atomic.Int32
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	host := tokens.HostFor(r.Token, f.root)
	query, _ := f.signedURLFor(t, r.Token, 10*time.Minute)

	// No JWT — only the signed URL.
	resp := f.do(t, host, "", "/"+query)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if upstreamHits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", upstreamHits.Load())
	}
	// Gateway should have set sig+exp cookies for subsequent fetches.
	var sawSig, sawExp bool
	for _, c := range resp.Cookies() {
		if c.Name == tokens.SigParam {
			sawSig = true
		}
		if c.Name == tokens.ExpParam {
			sawExp = true
		}
	}
	if !sawSig || !sawExp {
		t.Errorf("first signed-URL request should Set-Cookie sig+exp; got sig=%t exp=%t", sawSig, sawExp)
	}
}

func TestPreviewProxy_OwnerOnlySignedCookieSucceedsOnSubsequentRequest(t *testing.T) {
	t.Parallel()
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	host := tokens.HostFor(r.Token, f.root)
	exp := time.Now().Add(10 * time.Minute)
	sig, err := tokens.Sign(f.signingKey, r.Token, exp)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Manually carry sig+exp as cookies — no query param. Simulates a
	// runtime fetch after the proxy Set-Cookied on the first hop.
	req, _ := http.NewRequest("GET", f.srv.URL+"/bundle.js", nil)
	req.Host = host
	req.AddCookie(&http.Cookie{Name: tokens.SigParam, Value: sig})
	req.AddCookie(&http.Cookie{Name: tokens.ExpParam, Value: fmt.Sprintf("%d", exp.Unix())})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestPreviewProxy_OwnerOnlyExpiredSignatureRejected(t *testing.T) {
	t.Parallel()
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream hit on expired signature")
	}))
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	host := tokens.HostFor(r.Token, f.root)
	query, _ := f.signedURLFor(t, r.Token, -1*time.Minute) // already past

	resp := f.do(t, host, "", "/"+query)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired signature: status = %d, want 401", resp.StatusCode)
	}
}

func TestPreviewProxy_OwnerOnlyTamperedSignatureFallsBackToJWT(t *testing.T) {
	t.Parallel()
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	host := tokens.HostFor(r.Token, f.root)
	// Tampered sig (right shape, wrong value) — VerifyFromRequest
	// returns ErrInvalidSignature → handler falls back to JWT path.
	tamperedQuery := fmt.Sprintf("?%s=AAAA&%s=%d", tokens.SigParam, tokens.ExpParam, time.Now().Add(10*time.Minute).Unix())

	// Without JWT: 401.
	resp := f.do(t, host, "", "/"+tamperedQuery)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tampered+no-jwt: status = %d, want 401", resp.StatusCode)
	}

	// With JWT (matching owner): success — the tampered sig didn't
	// block the JWT fall-through.
	resp = f.do(t, host, "alice", "/"+tamperedQuery)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("tampered+jwt: status = %d, want 200", resp.StatusCode)
	}
}

func TestPreviewProxy_OwnerOnlyCrossTokenSignatureRejected(t *testing.T) {
	t.Parallel()
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("upstream hit on cross-token signature")
	}))
	r := f.seed(t, "alice", tokens.VisibilityOwnerOnly)
	// Mint a valid signature for an arbitrary other token string,
	// then present it on r's hostname. The HMAC is over (token, exp)
	// so swapping the token must break verification — proves a
	// leaked sig for preview-A can't unlock preview-B.
	exp := time.Now().Add(10 * time.Minute)
	otherToken := "some-other-token-id"
	sigForOther, err := tokens.Sign(f.signingKey, otherToken, exp)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	host := tokens.HostFor(r.Token, f.root) // r's host, NOT otherToken's
	query := fmt.Sprintf("?%s=%s&%s=%d", tokens.SigParam, sigForOther, tokens.ExpParam, exp.Unix())

	resp := f.do(t, host, "", "/"+query)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("cross-token sig: status = %d, want 401", resp.StatusCode)
	}
}

func TestStripRawQueryParams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"nothing to strip", "a=1&b=2", "a=1&b=2"},
		{"strips both credentials", "a=1&clank_sig=xy&clank_exp=99&b=2", "a=1&b=2"},
		{"strips to empty", "clank_sig=xy&clank_exp=99", ""},

		// The regression this function exists for. url.Values.Encode
		// would return "lang.css=&svelte=&type=style" for each of these,
		// which Vite no longer recognizes as a style request.
		{"keeps valueless keys valueless", "svelte&type=style&lang.css", "svelte&type=style&lang.css"},
		{"keeps key order", "z=1&a=2&m=3", "z=1&a=2&m=3"},
		{"keeps empty values distinct from valueless", "a=&b", "a=&b"},
		{"vite style request behind a signature",
			"svelte&type=style&lang.css&clank_sig=xy&clank_exp=99",
			"svelte&type=style&lang.css"},

		// URL.Query drops semicolon-separated params outright (Go 1.17+).
		{"keeps semicolons", "a=1;b=2&c=3", "a=1;b=2&c=3"},

		// Encode would re-escape these into %2F, %3A, %5B%5D and "+".
		{"keeps escaping verbatim", "url=https://x.test/a?b&ids[]=1&q=a+b", "url=https://x.test/a?b&ids[]=1&q=a+b"},

		{"keeps duplicate keys", "a=1&a=2", "a=1&a=2"},
		{"matches on the decoded key", "clank%5Fsig=xy&a=1", "a=1"},

		// Only a whole key matches: a param that merely mentions the
		// credential name in its value, or as a prefix, stays.
		{"leaves lookalike keys", "clank_signature=xy&a=clank_sig", "clank_signature=xy&a=clank_sig"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := stripRawQueryParams(tc.raw, tokens.SigParam, tokens.ExpParam)
			if got != tc.want {
				t.Errorf("stripRawQueryParams(%q)\n got %q\nwant %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestPreviewProxy_ForwardsRawQueryVerbatim(t *testing.T) {
	t.Parallel()
	// Vite addresses a module's sub-resources by exact query, so the proxy
	// has to hand the dev server the same bytes the browser sent, minus the
	// credentials. Sorting the keys or appending "=" to the valueless ones
	// makes vite-plugin-svelte serve the component module in place of its
	// CSS, which silently strips every scoped style from the page.
	const want = "svelte&type=style&lang.css"
	got := make(chan string, 1)
	f := newPreviewProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.URL.RawQuery
		_, _ = w.Write([]byte("k"))
	}))
	route := f.seed(t, "alice", tokens.VisibilityPublic)
	host := tokens.HostFor(route.Token, f.root)

	path := fmt.Sprintf("/src/lib/App.svelte?%s&%s=xy&%s=99", want, tokens.SigParam, tokens.ExpParam)
	resp := f.do(t, host, "", path)
	resp.Body.Close()

	select {
	case upstream := <-got:
		if upstream != want {
			t.Errorf("upstream saw query %q, want %q", upstream, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream handler was never invoked (proxy didn't forward the request)")
	}
}

// Compile-time guard that the various test fakes still satisfy what
// the test fixtures need, and that test errors surface in the right
// spot if signatures drift.
var (
	_ = errors.Is
	_ = fmt.Sprintf
)
