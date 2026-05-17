package cloud

// oauth.go — standards OAuth 2.0 Authorization Code + PKCE client
// (RFC 6749 + RFC 7636). Browser-based flow with a localhost callback;
// suitable for desktop CLIs where the user can open a browser. Knows
// nothing about any specific IdP: the endpoint URLs and client_id are
// supplied at construction time (typically by FetchAuthConfig).
//
// Token endpoint uses application/x-www-form-urlencoded per RFC 6749
// §4.1.3. PKCE uses S256 (SHA256(verifier), base64url) per RFC 7636.
//
// Not used in SSH / container environments: localhost callbacks
// can't reach the user's laptop from a remote shell. Device flow
// (RFC 8628) could be re-added as a fallback later (auto-detected via
// $SSH_TTY).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// OAuthClient runs the PKCE dance against a single IdP. Construct once
// per login attempt; not reused after Login returns.
type OAuthClient struct {
	// AuthorizeEndpoint is the IdP's authorize URL, e.g.
	// "https://abc.supabase.co/oauth/authorize" or
	// "https://auth.example.com/oauth/authorize".
	AuthorizeEndpoint string

	// TokenEndpoint is the IdP's token URL, e.g.
	// "https://abc.supabase.co/oauth/token".
	TokenEndpoint string

	// ClientID is the public OAuth client identifier registered with
	// the IdP. PKCE replaces the client secret, so no secret here.
	ClientID string

	// Scopes are the OAuth scopes requested at /authorize. Joined
	// with spaces per RFC 6749 §3.3. May be empty.
	Scopes []string

	// Provider is an optional IdP hint passed as the non-standard
	// `provider` query parameter on /authorize. Used by Supabase Auth
	// (and similar) to route the user straight to GitHub / Google /
	// etc. Ignored by IdPs that don't recognise it.
	Provider string

	// OpenBrowser is the function called to launch the user's
	// browser. Optional; defaults to the platform's standard
	// open command. Tests inject a no-op.
	OpenBrowser func(target string) error

	// CallbackHosts is the set of bind hosts the localhost listener
	// will try in order. Default ["127.0.0.1", "localhost"]; most
	// IdPs allow either as a wildcard in their redirect-URI config.
	CallbackHosts []string

	// CallbackPort, when non-zero, pins the localhost listener to
	// exactly this port. Default 0 = kernel-assigned random port,
	// which is what RFC 8252 §7.3 recommends for native apps.
	//
	// Set this when the IdP rejects redirect_uris with arbitrary
	// loopback ports (Supabase OAuth Server, for example, requires
	// an exact match against the registered redirect_uris). The
	// gateway communicates the required port via AuthConfig.
	// If the port is already in use, Login returns an error.
	CallbackPort int

	// HTTPClient is used for the token exchange. Optional;
	// defaults to a 30s-timeout client.
	HTTPClient *http.Client

	// Prompt receives the manual-paste URL when OpenBrowser fails so
	// the user can still complete the flow by opening the link in any
	// browser. nil → os.Stderr (visible by default for CLI callers).
	// Set to io.Discard from TUI contexts where stderr writes would
	// corrupt the display.
	Prompt io.Writer
}

// ErrLoginCancelled is returned when the user aborts the flow
// (closes the browser, or sends ^C in the parent context).
var ErrLoginCancelled = errors.New("cloud: login cancelled")

// ErrLoginTimeout is returned when the OAuth provider doesn't
// redirect back within the timeout window. Default 5 minutes
// (passed via ctx; oauth.go itself doesn't set a budget).
var ErrLoginTimeout = errors.New("cloud: login timed out")

// Login runs the full PKCE flow:
//
//  1. Generate code_verifier + code_challenge (S256).
//  2. Bind a localhost listener on a random free port.
//  3. Open the user's browser to AuthorizeEndpoint with PKCE params
//     and a redirect_uri pointing at the localhost listener.
//  4. Wait for the IdP's redirect (?code=... or ?error=...).
//  5. Exchange the code at TokenEndpoint (form-encoded).
//  6. Return the resulting Session (with sub/email decoded from the
//     JWT payload when the access token is a JWT).
//
// The context's deadline bounds the wait — caller is expected to
// pass ctx with a reasonable timeout (e.g. 5 minutes). The browser
// always opens; if OpenBrowser fails, the URL is returned in the
// error so the user can paste it manually.
func (c *OAuthClient) Login(ctx context.Context) (*Session, error) {
	if c.AuthorizeEndpoint == "" {
		return nil, fmt.Errorf("oauth: AuthorizeEndpoint is required")
	}
	if c.TokenEndpoint == "" {
		return nil, fmt.Errorf("oauth: TokenEndpoint is required")
	}
	if c.ClientID == "" {
		return nil, fmt.Errorf("oauth: ClientID is required")
	}
	openBrowser := c.OpenBrowser
	if openBrowser == nil {
		openBrowser = defaultOpenBrowser
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	verifier, challenge, err := generatePKCEPair()
	if err != nil {
		return nil, fmt.Errorf("oauth: generate PKCE pair: %w", err)
	}

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("oauth: generate state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	listener, redirectURL, err := bindLocalhost(c.CallbackHosts, c.CallbackPort)
	if err != nil {
		return nil, fmt.Errorf("oauth: bind localhost: %w", err)
	}
	defer listener.Close()

	authorizeURL := buildAuthorizeURL(c.AuthorizeEndpoint, c.ClientID, c.Scopes, c.Provider, redirectURL, challenge, state)

	// Channel for the callback handler to deliver the result.
	type callbackResult struct {
		code  string
		err   error
		state string
	}
	resultCh := make(chan callbackResult, 1)
	send := func(r callbackResult) {
		// Non-blocking: a second redirect (or a quirky provider) must
		// not pin the handler goroutine on a full buffered channel.
		select {
		case resultCh <- r:
		default:
		}
	}

	// Callback handler. Only the root path is the OAuth callback;
	// favicon and other browser-issued sub-requests get a 404 so
	// they can't render a misleading "no code" page or trip the
	// non-blocking guard on the result channel.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if errStr := q.Get("error"); errStr != "" {
			desc := q.Get("error_description")
			renderCallbackPage(w, false, "Login failed: "+errStr+" — "+desc)
			send(callbackResult{err: fmt.Errorf("oauth: %s: %s", errStr, desc)})
			return
		}
		code := q.Get("code")
		gotState := q.Get("state")
		if code == "" {
			renderCallbackPage(w, false, "Login failed: no code in callback.")
			send(callbackResult{err: fmt.Errorf("oauth: callback missing code")})
			return
		}
		if gotState != state {
			// Validate before rendering success so a mismatched
			// callback never tells the user "Signed in" while
			// Login is about to return an error.
			renderCallbackPage(w, false, "Login failed: state mismatch (possible CSRF).")
			send(callbackResult{err: fmt.Errorf("oauth: state mismatch (possible CSRF) — got %q, want %q", gotState, state)})
			return
		}
		renderCallbackPage(w, true, "")
		send(callbackResult{code: code, state: gotState})
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(listener) //nolint:errcheck — Close triggers a benign net.ErrClosed.
	defer srv.Close()

	// Best-effort: open the browser. If it fails, surface the URL
	// via Prompt and keep the loopback server alive — the user can
	// open the link manually on the same machine.
	if err := openBrowser(authorizeURL); err != nil {
		prompt := c.Prompt
		if prompt == nil {
			prompt = os.Stderr
		}
		fmt.Fprintf(prompt, "\nCould not open browser automatically (%v).\nOpen this URL to sign in:\n\n  %s\n\n", err, authorizeURL)
	}

	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrLoginTimeout
		}
		return nil, ErrLoginCancelled
	case res := <-resultCh:
		if res.err != nil {
			return nil, res.err
		}
		return c.exchangeCode(ctx, httpClient, res.code, verifier, redirectURL)
	}
}

// Refresh exchanges a refresh_token for a fresh access_token.
// Used when the cached access_token has expired but the refresh
// token is still valid.
func (c *OAuthClient) Refresh(ctx context.Context, refreshToken string) (*Session, error) {
	if c.TokenEndpoint == "" || c.ClientID == "" {
		return nil, fmt.Errorf("oauth: TokenEndpoint and ClientID are required")
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {c.ClientID},
	}
	return doTokenExchange(ctx, httpClient, c.TokenEndpoint, form.Encode())
}

// --- internals ------------------------------------------------------

func (c *OAuthClient) exchangeCode(ctx context.Context, hc *http.Client, code, verifier, redirectURI string) (*Session, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {c.ClientID},
		"code_verifier": {verifier},
	}
	return doTokenExchange(ctx, hc, c.TokenEndpoint, form.Encode())
}

// tokenResponse mirrors RFC 6749 §5.1's standard token response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

// oauthErrorResponse mirrors RFC 6749 §5.2's error response.
type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func doTokenExchange(ctx context.Context, hc *http.Client, tokenURL, formBody string) (*Session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(formBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		var errResp oauthErrorResponse
		_ = json.Unmarshal(respBody, &errResp)
		msg := errResp.ErrorDescription
		if msg == "" {
			msg = errResp.Error
		}
		if msg == "" {
			msg = strings.TrimSpace(string(respBody))
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: %s", ErrUnauthorized, msg)
		}
		return nil, fmt.Errorf("token exchange: status %d: %s", resp.StatusCode, msg)
	}

	var tok tokenResponse
	if err := json.Unmarshal(respBody, &tok); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("token exchange: empty access_token in response")
	}
	sub, email, exp := decodeJWTClaims(tok.AccessToken)
	expires := exp
	if expires == 0 && tok.ExpiresIn > 0 {
		expires = time.Now().Unix() + tok.ExpiresIn
	}
	return &Session{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		UserID:       sub,
		UserEmail:    email,
		ExpiresAt:    expires,
	}, nil
}

// decodeJWTClaims base64-decodes the middle segment of a JWT and
// extracts sub, email, and exp. Best-effort — opaque (non-JWT) tokens
// or unparseable payloads return zero values; the gateway re-verifies
// the signature on every request so we don't need to here.
func decodeJWTClaims(token string) (sub, email string, exp int64) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", "", 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some libraries emit padded base64 — try the padded variant.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", "", 0
		}
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Exp   int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", 0
	}
	return claims.Sub, claims.Email, claims.Exp
}

// generatePKCEPair returns (code_verifier, code_challenge) per
// RFC 7636 §4.1. Verifier is a 64-byte URL-safe base64 string
// (well within RFC's 43-128 char range). Challenge is base64url
// of SHA256(verifier).
func generatePKCEPair() (verifier, challenge string, err error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// bindLocalhost tries each host in `hosts` in turn (e.g. 127.0.0.1
// then localhost) and returns the first successful listener plus the
// redirect URL the IdP should call back to. Defaults the host list
// when empty.
//
// When port == 0, binds on a kernel-assigned random port (RFC 8252's
// recommendation for native apps). When port != 0, binds exactly that
// port — required for IdPs that match redirect_uris strictly (e.g.
// Supabase OAuth Server). If the fixed port is in use, returns the
// EADDRINUSE error so the caller can surface a clear message.
func bindLocalhost(hosts []string, port int) (net.Listener, string, error) {
	if len(hosts) == 0 {
		hosts = []string{"127.0.0.1", "localhost"}
	}
	var lastErr error
	for _, h := range hosts {
		addr := fmt.Sprintf("%s:%d", h, port) // port=0 → kernel-assigned
		l, err := net.Listen("tcp", addr)
		if err != nil {
			lastErr = err
			continue
		}
		gotPort := l.Addr().(*net.TCPAddr).Port
		redirect := fmt.Sprintf("http://%s:%d", h, gotPort)
		return l, redirect, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no callback hosts to try")
	}
	return nil, "", lastErr
}

// buildAuthorizeURL constructs the /authorize URL with PKCE parameters
// per RFC 6749 §4.1.1 + RFC 7636 §4.3. provider is an optional
// non-spec hint; emitted only when non-empty.
func buildAuthorizeURL(authorizeEndpoint, clientID string, scopes []string, provider, redirectURI, challenge, state string) string {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURI)
	if len(scopes) > 0 {
		v.Set("scope", strings.Join(scopes, " "))
	}
	v.Set("state", state)
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	if provider != "" {
		v.Set("provider", provider)
	}
	sep := "?"
	if strings.Contains(authorizeEndpoint, "?") {
		sep = "&"
	}
	return authorizeEndpoint + sep + v.Encode()
}

// renderCallbackPage writes a minimal HTML page to the browser
// telling the user the flow completed. Inlined CSS so we don't
// have to ship assets.
func renderCallbackPage(w http.ResponseWriter, success bool, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	title := "Signed in to clank"
	body := `<p>You can close this tab and return to your terminal.</p>`
	if !success {
		title = "Sign-in failed"
		body = `<p>Something went wrong: ` + escapeHTML(errMsg) + `</p><p>Return to your terminal and try again.</p>`
	}
	_, _ = io.WriteString(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>`+title+`</title>
<style>
body { font-family: -apple-system, system-ui, sans-serif; max-width: 480px; margin: 80px auto; padding: 0 24px; color: #1a1a1a; line-height: 1.5; }
h1 { font-size: 1.4rem; margin: 0 0 8px; }
p { color: #555; }
</style>
</head>
<body>
<h1>`+title+`</h1>`+body+`
</body>
</html>
`)
}

func escapeHTML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// defaultOpenBrowser is the fallback opener used when OAuthClient
// doesn't override OpenBrowser. macOS → `open`, Linux → `xdg-open`,
// Windows → rundll32. Returns an error if the command fails to start;
// the browser may still succeed after this returns nil (it's just
// `cmd.Start`, not `cmd.Run`).
func defaultOpenBrowser(target string) error {
	if target == "" {
		return fmt.Errorf("no URL to open")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
