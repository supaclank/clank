package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestAuthorize_RedirectsWithCode pins the /authorize half of the PKCE
// flow: given valid params, the stub auto-approves and 302s to the
// redirect_uri with code+state.
func TestAuthorize_RedirectsWithCode(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	verifier, challenge := freshPKCE(t)

	// HTTP client that doesn't follow redirects, so we can inspect Location.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {"clank-cli"},
		"redirect_uri":          {"http://localhost:12345/cb"},
		"state":                 {"xyz"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"openid email"},
	}
	resp, err := client.Get(s.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer closeBody(t, resp.Body)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: got %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("missing Location header")
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if u.Scheme != "http" || u.Host != "localhost:12345" || u.Path != "/cb" {
		t.Errorf("Location URL %q doesn't preserve redirect_uri", loc)
	}
	if got := u.Query().Get("state"); got != "xyz" {
		t.Errorf("state: got %q, want xyz", got)
	}
	if u.Query().Get("code") == "" {
		t.Error("missing code in redirect")
	}
	_ = verifier // used by the token-exchange test below
}

// TestTokenExchange_AuthorizationCode runs the full flow:
// /authorize → /token, verifies the JWT round-trip.
func TestTokenExchange_AuthorizationCode(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	verifier, challenge := freshPKCE(t)
	code := runAuthorize(t, s, challenge, "http://localhost:0/cb")

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://localhost:0/cb"},
		"client_id":     {"clank-cli"},
		"code_verifier": {verifier},
	}
	resp, err := http.Post(s.URL+"/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer closeBody(t, resp.Body)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("token status %d: %s", resp.StatusCode, body)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken == "" {
		t.Fatal("empty access_token")
	}
	if tok.TokenType != "Bearer" {
		t.Errorf("token_type: got %q, want Bearer", tok.TokenType)
	}
	if tok.ExpiresIn <= 0 {
		t.Errorf("expires_in: got %d, want positive", tok.ExpiresIn)
	}

	// /me with the bearer must work — proves JWT is well-formed and
	// verifies against the same secret the stub used to sign it.
	req, _ := http.NewRequest(http.MethodGet, s.URL+"/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	mr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	defer closeBody(t, mr.Body)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("me status %d", mr.StatusCode)
	}
}

// TestTokenExchange_PKCEMismatch pins that a wrong verifier fails.
func TestTokenExchange_PKCEMismatch(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	_, challenge := freshPKCE(t)
	code := runAuthorize(t, s, challenge, "http://localhost:0/cb")

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://localhost:0/cb"},
		"client_id":     {"clank-cli"},
		"code_verifier": {"wrong-verifier"},
	}
	resp, err := http.Post(s.URL+"/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer closeBody(t, resp.Body)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("token exchange with wrong verifier should fail")
	}
}

// TestTokenExchange_CodeIsSingleUse pins single-use semantics — a
// stolen-and-replayed code can't mint a second token.
func TestTokenExchange_CodeIsSingleUse(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	verifier, challenge := freshPKCE(t)
	code := runAuthorize(t, s, challenge, "http://localhost:0/cb")

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://localhost:0/cb"},
		"client_id":     {"clank-cli"},
		"code_verifier": {verifier},
	}
	body := strings.NewReader(form.Encode())
	resp1, err := http.Post(s.URL+"/token", "application/x-www-form-urlencoded", body)
	if err != nil {
		t.Fatalf("token #1: %v", err)
	}
	closeBody(t, resp1.Body)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first redemption failed: status %d", resp1.StatusCode)
	}

	resp2, err := http.Post(s.URL+"/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("token #2: %v", err)
	}
	defer closeBody(t, resp2.Body)
	if resp2.StatusCode == http.StatusOK {
		t.Fatal("second redemption with same code should fail")
	}
}

// TestRefreshToken_AutoApproves pins that the refresh path mints a new
// token, and the refresh token is single-use.
func TestRefreshToken_AutoApproves(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	verifier, challenge := freshPKCE(t)
	code := runAuthorize(t, s, challenge, "http://localhost:0/cb")

	tok := runTokenExchange(t, s, code, verifier, "http://localhost:0/cb")
	if tok.RefreshToken == "" {
		t.Fatal("no refresh_token in initial response")
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tok.RefreshToken},
	}
	resp, err := http.Post(s.URL+"/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer closeBody(t, resp.Body)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("refresh status %d: %s", resp.StatusCode, body)
	}

	// Replay should fail — single-use.
	resp2, err := http.Post(s.URL+"/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("refresh #2: %v", err)
	}
	defer closeBody(t, resp2.Body)
	if resp2.StatusCode == http.StatusOK {
		t.Fatal("replay of refresh_token should fail")
	}
}

// --- helpers --------------------------------------------------------

// closeBody is the canonical body-close in tests: surface the error
// via t.Errorf so errcheck stays happy and any actual close failure
// gets noticed instead of swallowed.
func closeBody(t *testing.T, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := config{
		listen:   ":0",
		secret:   []byte("test-secret"),
		userID:   "dev-user",
		email:    "dev@clank.local",
		tokenTTL: time.Minute,
	}
	s := newServer(cfg)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	cfg.publicURL = srv.URL
	s.cfg = cfg
	return srv
}

func freshPKCE(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = "test-verifier-with-enough-entropy-to-pass-the-length-check"
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func runAuthorize(t *testing.T, srv *httptest.Server, challenge, redirect string) string {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {"clank-cli"},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	resp, err := client.Get(srv.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	closeBody(t, resp.Body)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status %d", resp.StatusCode)
	}
	u, _ := url.Parse(resp.Header.Get("Location"))
	code := u.Query().Get("code")
	if code == "" {
		t.Fatal("no code in redirect")
	}
	return code
}

func runTokenExchange(t *testing.T, srv *httptest.Server, code, verifier, redirect string) struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
} {
	t.Helper()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {"clank-cli"},
		"code_verifier": {verifier},
	}
	resp, err := http.Post(srv.URL+"/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer closeBody(t, resp.Body)
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		t.Fatal(err)
	}
	return tok
}
