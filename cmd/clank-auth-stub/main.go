// clank-auth-stub is a minimal RFC 8628 (OAuth 2.0 Device Authorization
// Grant) server for clank dev/testing.
//
// It auto-approves every device flow and mints an HS256-signed JWT
// against a configurable secret. Stand it up next to a clankd dev
// stack so `clank login` works end-to-end without an external auth
// provider — useful for local smoke tests of the whole CLI flow.
//
// Not for production use: there is no real user authentication; every
// device code is approved unconditionally.
//
// Env (all optional with sensible defaults):
//
//	CLANK_AUTH_STUB_LISTEN     — bind address. Default ":7879".
//	CLANK_AUTH_STUB_PUBLIC_URL — externally-reachable base URL embedded
//	                              into verification_uri responses.
//	                              Default "http://localhost:<port>".
//	CLANK_AUTH_STUB_SECRET     — HMAC-SHA256 secret. Default "dev-secret".
//	CLANK_AUTH_STUB_USER_ID    — sub claim. Default "dev-user".
//	CLANK_AUTH_STUB_EMAIL      — email claim. Default "dev@clank.local".
//	CLANK_AUTH_STUB_TTL        — token TTL. Default 1h (Go duration).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
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

// deviceCode tracks one active device-flow grant. Since the stub
// auto-approves, the in-memory map is short-lived; a 15-minute reaper
// trims abandoned codes.
type deviceCode struct {
	createdAt time.Time
	expiresAt time.Time
}

type server struct {
	cfg config

	mu      sync.Mutex
	devices map[string]deviceCode // keyed by device_code
}

func newServer(cfg config) *server {
	return &server{cfg: cfg, devices: map[string]deviceCode{}}
}

func (s *server) Handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("POST /auth/device/start", s.handleDeviceStart)
	mx.HandleFunc("POST /auth/device/poll", s.handleDevicePoll)
	mx.HandleFunc("POST /auth/signout", s.handleSignOut)
	mx.HandleFunc("GET /me", s.handleMe)
	mx.HandleFunc("GET /verify", s.handleVerify)
	mx.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mx
}

// handleDeviceStart mints a fresh (device_code, user_code) pair. The
// stub doesn't bother making user_code human-typable in groups — dev
// users either follow the verification_uri_complete link or paste the
// code into the auto-approval page.
func (s *server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	deviceCodeStr := randHex(16)
	userCode := strings.ToUpper(randHex(4))
	now := time.Now()
	s.mu.Lock()
	s.devices[deviceCodeStr] = deviceCode{
		createdAt: now,
		expiresAt: now.Add(15 * time.Minute),
	}
	s.mu.Unlock()

	verify := s.cfg.publicURL + "/verify?user_code=" + userCode
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               deviceCodeStr,
		"user_code":                 userCode,
		"verification_uri":          s.cfg.publicURL + "/verify",
		"verification_uri_complete": verify,
		"expires_in":                900,
		"interval":                  2,
	})
}

// handleDevicePoll auto-approves: the first poll returns a signed JWT.
// A real RFC 8628 server would return "authorization_pending" until
// the user clicked through; we skip that to keep the dev loop tight.
func (s *server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	s.mu.Lock()
	rec, ok := s.devices[body.DeviceCode]
	if ok && time.Now().After(rec.expiresAt) {
		delete(s.devices, body.DeviceCode)
		ok = false
	}
	if ok {
		delete(s.devices, body.DeviceCode) // single-use
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expired_token"})
		return
	}

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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sign_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  tok,
		"refresh_token": randHex(16),
		"token_type":    "Bearer",
		"expires_in":    int(s.cfg.tokenTTL.Seconds()),
		"user_id":       s.cfg.userID,
		"email":         s.cfg.email,
	})
}

// handleMe verifies the bearer and returns the static user profile.
// Cloud /me responses also include organisations/hubs/hosts; we return
// empty arrays so the TUI's MeResponse decoder is happy.
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

// handleVerify is the user-facing browser landing for the
// verification_uri. The stub auto-approves so there's nothing for the
// user to click; we just acknowledge the code is approved.
func (s *server) handleVerify(w http.ResponseWriter, r *http.Request) {
	userCode := r.URL.Query().Get("user_code")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><html><body style="font-family:system-ui;padding:2em;max-width:36em;margin:auto;">
<h2>clank auth-stub</h2>
<p>Code <code>%s</code> is auto-approved. Return to your terminal — <code>clank login</code> should complete on the next poll.</p>
</body></html>`, htmlEscape(userCode))
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

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
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
