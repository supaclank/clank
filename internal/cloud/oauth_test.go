package cloud

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestLogin_EndToEnd walks a complete PKCE round-trip against a tiny
// in-process spec-compliant IdP. Asserts the wire format clank emits:
// /authorize query params (response_type, client_id, redirect_uri,
// scope, state, code_challenge, code_challenge_method=S256) and the
// /token form body (grant_type=authorization_code, code, redirect_uri,
// client_id, code_verifier).
func TestLogin_EndToEnd(t *testing.T) {
	t.Parallel()

	const (
		clientID = "test-cli"
		secret   = "test-secret"
		sub      = "u-42"
		email    = "u@example.com"
	)

	var (
		authorizeParams url.Values
		tokenForm       url.Values
		issuedCode      = "abc123"
		expectedScopes  = "openid email"
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		authorizeParams = r.URL.Query()
		// 302 back to the redirect_uri with code + state.
		redirect := authorizeParams.Get("redirect_uri")
		state := authorizeParams.Get("state")
		u, _ := url.Parse(redirect)
		q := u.Query()
		q.Set("code", issuedCode)
		q.Set("state", state)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type: got %q, want application/x-www-form-urlencoded", got)
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tokenForm = r.PostForm

		// Mint a JWT-shaped access token so decodeJWTClaims can read sub/email.
		jwt := signTestJWT(t, []byte(secret), map[string]any{
			"sub":   sub,
			"email": email,
			"exp":   time.Now().Add(time.Hour).Unix(),
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  jwt,
			"refresh_token": "rt-1",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Capture the URL the browser would be opened to; trigger the
	// callback ourselves with an HTTP GET so the listener accepts it.
	openedCh := make(chan string, 1)
	openBrowser := func(target string) error {
		openedCh <- target
		return nil
	}

	cli := &OAuthClient{
		AuthorizeEndpoint: srv.URL + "/authorize",
		TokenEndpoint:     srv.URL + "/token",
		ClientID:          clientID,
		Scopes:            []string{"openid", "email"},
		OpenBrowser:       openBrowser,
	}

	// Drive the IdP redirect from a goroutine: once Login opens the
	// browser, follow the authorize URL with a permissive http.Client
	// so the IdP's 302 hits our listener.
	go func() {
		target := <-openedCh
		c := &http.Client{Timeout: 5 * time.Second}
		getAndDiscard(c, target) // follows redirects to localhost listener
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := cli.Login(ctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// --- Assertions on /authorize wire format -----------------------
	if got := authorizeParams.Get("response_type"); got != "code" {
		t.Errorf("authorize response_type: got %q, want code", got)
	}
	if got := authorizeParams.Get("client_id"); got != clientID {
		t.Errorf("authorize client_id: got %q, want %q", got, clientID)
	}
	if got := authorizeParams.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method: got %q, want S256", got)
	}
	if got := authorizeParams.Get("code_challenge"); got == "" {
		t.Error("missing code_challenge")
	}
	if got := authorizeParams.Get("state"); got == "" {
		t.Error("missing state")
	}
	if got := authorizeParams.Get("scope"); got != expectedScopes {
		t.Errorf("scope: got %q, want %q", got, expectedScopes)
	}
	if got := authorizeParams.Get("redirect_uri"); !strings.HasPrefix(got, "http://") {
		t.Errorf("redirect_uri: got %q, want http://...", got)
	}

	// --- Assertions on /token wire format ---------------------------
	if got := tokenForm.Get("grant_type"); got != "authorization_code" {
		t.Errorf("grant_type: got %q, want authorization_code", got)
	}
	if got := tokenForm.Get("code"); got != issuedCode {
		t.Errorf("code: got %q, want %q", got, issuedCode)
	}
	if got := tokenForm.Get("client_id"); got != clientID {
		t.Errorf("client_id: got %q, want %q", got, clientID)
	}
	if got := tokenForm.Get("code_verifier"); got == "" {
		t.Error("missing code_verifier")
	}
	if got := tokenForm.Get("redirect_uri"); got == "" {
		t.Error("missing redirect_uri")
	}

	// --- Session decoded from JWT ----------------------------------
	if sess.UserID != sub {
		t.Errorf("UserID: got %q, want %q", sess.UserID, sub)
	}
	if sess.UserEmail != email {
		t.Errorf("UserEmail: got %q, want %q", sess.UserEmail, email)
	}
	if sess.RefreshToken != "rt-1" {
		t.Errorf("RefreshToken: got %q, want rt-1", sess.RefreshToken)
	}
}

// TestLogin_StateMismatchIsRejected pins CSRF protection — a callback
// arriving with a different state value than we sent must error.
func TestLogin_StateMismatchIsRejected(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		// Redirect with a state we did NOT send.
		redirect := r.URL.Query().Get("redirect_uri")
		u, _ := url.Parse(redirect)
		q := u.Query()
		q.Set("code", "x")
		q.Set("state", "ATTACKER")
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	openedCh := make(chan string, 1)
	cli := &OAuthClient{
		AuthorizeEndpoint: srv.URL + "/authorize",
		TokenEndpoint:     srv.URL + "/token", // unused — error before exchange
		ClientID:          "test",
		OpenBrowser:       func(t string) error { openedCh <- t; return nil },
	}
	go func() {
		target := <-openedCh
		c := &http.Client{Timeout: 5 * time.Second}
		getAndDiscard(c, target)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Login(ctx); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("expected state mismatch error, got %v", err)
	}
}

// TestLogin_StateMismatchRendersFailurePage pins the UX contract: a
// mismatched-state callback must show the user a failure page, not
// "Signed in to clank" followed by an error in the terminal.
// Validation must happen in the handler, before renderCallbackPage.
func TestLogin_StateMismatchRendersFailurePage(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		redirect := r.URL.Query().Get("redirect_uri")
		u, _ := url.Parse(redirect)
		q := u.Query()
		q.Set("code", "x")
		q.Set("state", "ATTACKER")
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	type pageCapture struct {
		status int
		body   string
	}
	pageCh := make(chan pageCapture, 1)
	openedCh := make(chan string, 1)
	cli := &OAuthClient{
		AuthorizeEndpoint: srv.URL + "/authorize",
		TokenEndpoint:     srv.URL + "/token",
		ClientID:          "test",
		OpenBrowser:       func(t string) error { openedCh <- t; return nil },
	}
	go func() {
		target := <-openedCh
		c := &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Stop on the localhost redirect so we can inspect
				// the page the browser would render to the user.
				if strings.HasPrefix(req.URL.String(), "http://127.0.0.1") ||
					strings.HasPrefix(req.URL.String(), "http://localhost") {
					return http.ErrUseLastResponse
				}
				return nil
			},
		}
		// First follow the IdP redirect to get the localhost URL.
		resp, err := c.Get(target)
		if err != nil {
			pageCh <- pageCapture{status: -1, body: err.Error()}
			return
		}
		loc, _ := resp.Location()
		_ = resp.Body.Close()
		// Now hit the localhost callback and capture the page body.
		resp2, err := c.Get(loc.String())
		if err != nil {
			pageCh <- pageCapture{status: -1, body: err.Error()}
			return
		}
		body, _ := io.ReadAll(resp2.Body)
		_ = resp2.Body.Close()
		pageCh <- pageCapture{status: resp2.StatusCode, body: string(body)}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cli.Login(ctx)
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("expected state mismatch error, got %v", err)
	}
	page := <-pageCh
	if page.status != http.StatusOK {
		t.Fatalf("page status = %d, want 200 (failure page is still rendered as 200 OK)", page.status)
	}
	if strings.Contains(page.body, "Signed in to clank") {
		t.Errorf("failure page leaked success copy: %s", page.body)
	}
	if !strings.Contains(page.body, "Sign-in failed") {
		t.Errorf("failure page missing failure copy: %s", page.body)
	}
}

// TestLogin_BrowserOpenFailureKeepsServerAlive pins the contract:
// when openBrowser fails, Login must NOT return — the loopback
// server stays up so the user can paste the URL into another
// browser and complete the flow. The fallback URL is surfaced via
// the Prompt writer.
func TestLogin_BrowserOpenFailureKeepsServerAlive(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		redirect := r.URL.Query().Get("redirect_uri")
		state := r.URL.Query().Get("state")
		u, _ := url.Parse(redirect)
		q := u.Query()
		q.Set("code", "manual-paste-ok")
		q.Set("state", state)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  "tok",
			"refresh_token": "rt",
			"token_type":    "Bearer",
			"expires_in":    60,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var prompt strings.Builder
	cli := &OAuthClient{
		AuthorizeEndpoint: srv.URL + "/authorize",
		TokenEndpoint:     srv.URL + "/token",
		ClientID:          "test",
		OpenBrowser:       func(string) error { return fmt.Errorf("no DISPLAY") },
		Prompt:            &prompt,
	}
	// Simulate the human pasting the URL: drive the redirect chain
	// once the prompt shows up.
	driveOnce := make(chan struct{})
	go func() {
		// Poll Prompt until the URL appears, then drive it through.
		for {
			s := prompt.String()
			if idx := strings.Index(s, "http://"); idx >= 0 {
				// The URL spans from "http://" to the first whitespace.
				rest := s[idx:]
				end := strings.IndexAny(rest, " \t\n")
				if end > 0 {
					target := rest[:end]
					c := &http.Client{Timeout: 5 * time.Second}
					getAndDiscard(c, target)
					close(driveOnce)
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := cli.Login(ctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	<-driveOnce
	if sess == nil || sess.AccessToken != "tok" {
		t.Fatalf("session: got %+v, want AccessToken=tok", sess)
	}
	if !strings.Contains(prompt.String(), "Could not open browser") {
		t.Errorf("Prompt missing fallback notice: %q", prompt.String())
	}
	if !strings.Contains(prompt.String(), "http://") {
		t.Errorf("Prompt missing authorize URL: %q", prompt.String())
	}
}

// TestRefresh_FormEncoded pins that Refresh emits the spec body shape
// (form-encoded, grant_type=refresh_token, refresh_token, client_id).
func TestRefresh_FormEncoded(t *testing.T) {
	t.Parallel()
	var gotForm url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"token_type":    "Bearer",
			"expires_in":    60,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cli := &OAuthClient{
		AuthorizeEndpoint: srv.URL + "/authorize",
		TokenEndpoint:     srv.URL + "/token",
		ClientID:          "client-x",
	}
	_, err := cli.Refresh(context.Background(), "old-rt")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := gotForm.Get("grant_type"); got != "refresh_token" {
		t.Errorf("grant_type: got %q, want refresh_token", got)
	}
	if got := gotForm.Get("refresh_token"); got != "old-rt" {
		t.Errorf("refresh_token: got %q, want old-rt", got)
	}
	if got := gotForm.Get("client_id"); got != "client-x" {
		t.Errorf("client_id: got %q, want client-x", got)
	}
}

// TestDecodeJWTClaims_ExtractsSubEmailExp covers the JWT-payload
// decoder used to populate Session.UserID/UserEmail from the access
// token. Signature is not verified — gateway does that.
func TestDecodeJWTClaims_ExtractsSubEmailExp(t *testing.T) {
	t.Parallel()
	tok := signTestJWT(t, []byte("ignored"), map[string]any{
		"sub":   "user-1",
		"email": "u@x.com",
		"exp":   int64(123456),
	})
	sub, email, exp := decodeJWTClaims(tok)
	if sub != "user-1" {
		t.Errorf("sub: got %q, want user-1", sub)
	}
	if email != "u@x.com" {
		t.Errorf("email: got %q, want u@x.com", email)
	}
	if exp != 123456 {
		t.Errorf("exp: got %d, want 123456", exp)
	}

	// Non-JWT opaque tokens decode to zero values, not a panic.
	sub2, _, _ := decodeJWTClaims("opaque-token")
	if sub2 != "" {
		t.Errorf("opaque token: got sub %q, want empty", sub2)
	}
}

// TestLogin_IgnoresNonRootCallbackRequests pins that a follow-up
// browser fetch to the localhost listener (e.g. /favicon.ico) gets a
// clean 404 without rendering a misleading "no code" page or blocking
// the handler goroutine on the result channel.
func TestLogin_IgnoresNonRootCallbackRequests(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		redirect := r.URL.Query().Get("redirect_uri")
		state := r.URL.Query().Get("state")
		u, _ := url.Parse(redirect)
		q := u.Query()
		q.Set("code", "abc")
		q.Set("state", state)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  "opaque-token",
			"refresh_token": "rt",
			"token_type":    "Bearer",
			"expires_in":    60,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	openedCh := make(chan string, 1)
	favStatusCh := make(chan int, 1)
	cli := &OAuthClient{
		AuthorizeEndpoint: srv.URL + "/authorize",
		TokenEndpoint:     srv.URL + "/token",
		ClientID:          "test",
		OpenBrowser:       func(t string) error { openedCh <- t; return nil },
	}
	go func() {
		target := <-openedCh
		u, _ := url.Parse(target)
		redirect := u.Query().Get("redirect_uri")
		state := u.Query().Get("state")
		c := &http.Client{Timeout: 5 * time.Second}
		// Browser fires favicon BEFORE the real callback — must
		// return 404 without engaging the result channel.
		resp, err := c.Get(redirect + "/favicon.ico")
		if err != nil {
			favStatusCh <- -1
		} else {
			favStatusCh <- resp.StatusCode
			resp.Body.Close()
		}
		// Now the real callback.
		q := url.Values{"code": {"abc"}, "state": {state}}
		getAndDiscard(c, redirect+"?"+q.Encode())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Login(ctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got := <-favStatusCh; got != http.StatusNotFound {
		t.Errorf("favicon status: got %d, want 404", got)
	}
}

// TestLogin_FixedCallbackPort pins the IdP-specifies-port mechanism:
// when OAuthClient.CallbackPort is set, the listener binds exactly
// that port and the redirect_uri sent to /authorize uses it.
// Required by IdPs that match redirect_uris strictly (Supabase OAuth
// Server is the motivating case).
func TestLogin_FixedCallbackPort(t *testing.T) {
	t.Parallel()
	// Bind a temp listener to discover an unused port we can request.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	wantPort := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	var gotRedirectURI string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		gotRedirectURI = r.URL.Query().Get("redirect_uri")
		redirect := r.URL.Query().Get("redirect_uri")
		state := r.URL.Query().Get("state")
		u, _ := url.Parse(redirect)
		q := u.Query()
		q.Set("code", "ok")
		q.Set("state", state)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  signTestJWT(t, []byte("k"), map[string]any{"sub": "u"}),
			"refresh_token": "rt",
			"token_type":    "Bearer",
			"expires_in":    60,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	openedCh := make(chan string, 1)
	cli := &OAuthClient{
		AuthorizeEndpoint: srv.URL + "/authorize",
		TokenEndpoint:     srv.URL + "/token",
		ClientID:          "test",
		CallbackPort:      wantPort,
		OpenBrowser:       func(target string) error { openedCh <- target; return nil },
	}
	go func() {
		target := <-openedCh
		c := &http.Client{Timeout: 5 * time.Second}
		getAndDiscard(c, target)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Login(ctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	wantSubstr := fmt.Sprintf(":%d", wantPort)
	if !strings.Contains(gotRedirectURI, wantSubstr) {
		t.Errorf("redirect_uri: got %q, want it to contain %q", gotRedirectURI, wantSubstr)
	}
}

// TestLogin_ErrorResponseBubbles pins that an IdP-returned OAuth error
// surfaces as a non-nil error to the caller.
func TestLogin_ErrorResponseBubbles(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		redirect := r.URL.Query().Get("redirect_uri")
		state := r.URL.Query().Get("state")
		u, _ := url.Parse(redirect)
		q := u.Query()
		q.Set("error", "access_denied")
		q.Set("error_description", "user said no")
		q.Set("state", state)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	openedCh := make(chan string, 1)
	cli := &OAuthClient{
		AuthorizeEndpoint: srv.URL + "/authorize",
		TokenEndpoint:     srv.URL + "/token",
		ClientID:          "test",
		OpenBrowser:       func(t string) error { openedCh <- t; return nil },
	}
	go func() {
		target := <-openedCh
		c := &http.Client{Timeout: 5 * time.Second}
		getAndDiscard(c, target)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cli.Login(ctx)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("expected access_denied error, got %v", err)
	}
}

// --- helpers --------------------------------------------------------

// getAndDiscard issues a GET and immediately closes the body. Used by
// the IdP-driver goroutines whose only job is to walk the redirect
// chain into clank's localhost listener; the body bytes are noise.
// Keeps the test sockets from leaking file descriptors when these
// tests are run in a tight loop.
func getAndDiscard(c *http.Client, url string) {
	resp, err := c.Get(url)
	if err == nil && resp != nil {
		_ = resp.Body.Close()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// signTestJWT produces a tiny HS256 JWT with the given payload claims.
// Sufficient for decodeJWTClaims tests; no real verification done by
// the cloud package.
func signTestJWT(t *testing.T, secret []byte, claims map[string]any) string {
	t.Helper()
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(claims)
	h := base64.RawURLEncoding.EncodeToString(hb)
	p := base64.RawURLEncoding.EncodeToString(pb)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(h + "." + p))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s.%s.%s", h, p, sig)
}
