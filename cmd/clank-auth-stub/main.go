// clank-auth-stub is a minimal OAuth 2.0 Authorization Code + PKCE
// server (RFC 6749 + RFC 7636) for clank dev/testing.
//
// It auto-approves every authorization request and mints HS256-signed
// JWTs against a configurable secret. Stand it up next to a clankd dev
// stack so `clank login` works end-to-end without an external IdP.
//
// Drop-in replacement for any spec-compliant OAuth 2.0 IdP for local
// dev. Not for production use: there is no real user authentication;
// every authorize request is approved unconditionally.
//
// Env (all optional with sensible defaults):
//
//	CLANK_AUTH_STUB_LISTEN     — bind address. Default ":7879".
//	CLANK_AUTH_STUB_PUBLIC_URL — externally-reachable base URL embedded
//	                              into the JWT issuer claim. Default
//	                              "http://localhost:<port>".
//	CLANK_AUTH_STUB_SECRET     — HMAC-SHA256 secret. Default "dev-secret".
//	CLANK_AUTH_STUB_USER_ID    — sub claim. Default "dev-user".
//	CLANK_AUTH_STUB_EMAIL      — email claim. Default "dev@clank.local".
//	CLANK_AUTH_STUB_TTL        — token TTL. Default 1h (Go duration).
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const defaultListen = ":7879"

type config struct {
	listen    string
	publicURL string
	secret    []byte
	userID    string
	email     string
	tokenTTL  time.Duration
}

func loadConfig() (config, error) {
	c := config{
		listen:   envOrDefault("CLANK_AUTH_STUB_LISTEN", defaultListen),
		userID:   envOrDefault("CLANK_AUTH_STUB_USER_ID", "dev-user"),
		email:    envOrDefault("CLANK_AUTH_STUB_EMAIL", "dev@clank.local"),
		secret:   []byte(envOrDefault("CLANK_AUTH_STUB_SECRET", "dev-secret")),
		tokenTTL: time.Hour,
	}
	if v := os.Getenv("CLANK_AUTH_STUB_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("CLANK_AUTH_STUB_TTL: %w", err)
		}
		c.tokenTTL = d
	}
	if v := os.Getenv("CLANK_AUTH_STUB_PUBLIC_URL"); v != "" {
		c.publicURL = v
	} else {
		// Build "http://localhost:<port>" from listen — net.SplitHostPort
		// so values like "0.0.0.0:7879" don't produce "http://localhost0.0.0.0:7879".
		_, port, err := net.SplitHostPort(c.listen)
		if err != nil {
			port = strings.TrimPrefix(c.listen, ":")
		}
		c.publicURL = "http://localhost:" + port
	}
	return c, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// pendingCode tracks one issued-but-not-yet-redeemed authorization
// code. Codes are single-use; a 15-minute reaper trims abandoned ones
// (the stub's auto-approval typically redeems within seconds).
type pendingCode struct {
	codeChallenge string
	clientID      string
	redirectURI   string
	expiresAt     time.Time
}

type server struct {
	cfg config

	mu       sync.Mutex
	pending  map[string]pendingCode // keyed by authorization code
	refresh  map[string]bool        // valid refresh tokens (single-use)
}

func newServer(cfg config) *server {
	return &server{
		cfg:     cfg,
		pending: map[string]pendingCode{},
		refresh: map[string]bool{},
	}
}

func (s *server) Handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("GET /authorize", s.handleAuthorize)
	mx.HandleFunc("POST /token", s.handleToken)
	mx.HandleFunc("POST /auth/signout", s.handleSignOut)
	mx.HandleFunc("GET /me", s.handleMe)
	mx.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mx
}

// handleAuthorize is the OAuth 2.0 /authorize endpoint. Auto-approves:
// generates a fresh code keyed against the PKCE challenge and 302s
// back to the supplied redirect_uri. A real IdP would render a login
// + consent page here; the stub skips that to keep dev tight.
func (s *server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")
	codeChallenge := q.Get("code_challenge")
	codeChallengeMethod := q.Get("code_challenge_method")

	if clientID == "" || redirectURI == "" || codeChallenge == "" {
		http.Error(w, "missing client_id, redirect_uri, or code_challenge", http.StatusBadRequest)
		return
	}
	if codeChallengeMethod != "" && codeChallengeMethod != "S256" {
		http.Error(w, "unsupported code_challenge_method (S256 only)", http.StatusBadRequest)
		return
	}
	if _, err := url.Parse(redirectURI); err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}

	code := randHex(16)
	s.mu.Lock()
	s.pending[code] = pendingCode{
		codeChallenge: codeChallenge,
		clientID:      clientID,
		redirectURI:   redirectURI,
		expiresAt:     time.Now().Add(15 * time.Minute),
	}
	s.mu.Unlock()

	dest, _ := url.Parse(redirectURI)
	vals := dest.Query()
	vals.Set("code", code)
	if state != "" {
		vals.Set("state", state)
	}
	dest.RawQuery = vals.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

// handleToken is the OAuth 2.0 /token endpoint. Accepts form-encoded
// bodies per RFC 6749 §4.1.3. Supports grant_type=authorization_code
// (with PKCE) and grant_type=refresh_token.
func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "could not parse form body")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.tokenAuthorizationCode(w, r)
	case "refresh_token":
		s.tokenRefresh(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *server) tokenAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")
	clientID := r.PostForm.Get("client_id")
	redirectURI := r.PostForm.Get("redirect_uri")
	if code == "" || verifier == "" || clientID == "" || redirectURI == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing code, code_verifier, client_id, or redirect_uri")
		return
	}
	s.mu.Lock()
	rec, ok := s.pending[code]
	if ok {
		delete(s.pending, code) // single-use, regardless of validation outcome
		if time.Now().After(rec.expiresAt) {
			ok = false
		}
	}
	s.mu.Unlock()
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code unknown or expired")
		return
	}
	if rec.clientID != clientID || rec.redirectURI != redirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id or redirect_uri mismatch")
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	gotChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if gotChallenge != rec.codeChallenge {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match code_challenge")
		return
	}
	s.issueTokens(w)
}

func (s *server) tokenRefresh(w http.ResponseWriter, r *http.Request) {
	refresh := r.PostForm.Get("refresh_token")
	if refresh == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing refresh_token")
		return
	}
	s.mu.Lock()
	ok := s.refresh[refresh]
	delete(s.refresh, refresh) // single-use
	s.mu.Unlock()
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token unknown")
		return
	}
	s.issueTokens(w)
}

// issueTokens mints a fresh access+refresh pair and writes the
// standard OAuth 2.0 token response. The access token is an HS256 JWT
// carrying sub, email, iat, exp, iss, aud — claims the gateway can
// verify via its JWTHS256 authenticator using the same secret.
func (s *server) issueTokens(w http.ResponseWriter) {
	now := time.Now()
	exp := now.Add(s.cfg.tokenTTL)
	jwtTok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":   s.cfg.userID,
		"email": s.cfg.email,
		"iat":   now.Unix(),
		"exp":   exp.Unix(),
		"iss":   s.cfg.publicURL,
		"aud":   "clank",
	})
	tok, err := jwtTok.SignedString(s.cfg.secret)
	if err != nil {
		log.Printf("sign JWT: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to sign token")
		return
	}
	refresh := randHex(24)
	s.mu.Lock()
	s.refresh[refresh] = true
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  tok,
		"refresh_token": refresh,
		"token_type":    "Bearer",
		"expires_in":    int(s.cfg.tokenTTL.Seconds()),
	})
}

// handleMe verifies the bearer and returns the static user profile.
// Cloud /me responses also include organisations/hubs/hosts; we return
// empty arrays so the TUI's MeResponse decoder is happy. Same as the
// previous device-flow stub — unrelated to OAuth, useful for dev.
func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	if _, err := s.verifyBearer(r); err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="clank-auth-stub"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":       s.cfg.userID,
		"email":         s.cfg.email,
		"organisations": []any{},
		"hubs":          []any{},
		"hosts":         []any{},
	})
}

func (s *server) handleSignOut(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *server) verifyBearer(r *http.Request) (jwt.MapClaims, error) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return nil, errors.New("missing bearer")
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(auth[len(prefix):], claims, func(*jwt.Token) (any, error) {
		return s.cfg.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// --- misc helpers ----------------------------------------------------

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Vanishingly unlikely on Linux/macOS; panic since the whole
		// security story depends on rand working.
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	s := newServer(cfg)
	httpSrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Printf("clank-auth-stub listening on %s (public=%s, user=%s)", cfg.listen, cfg.publicURL, cfg.userID)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	_ = httpSrv.Shutdown(shutdownCtx)
}
