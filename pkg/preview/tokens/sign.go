package tokens

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Signed-URL bearer for owner-only preview access from clients that
// can't inject an Authorization header (Expo's dev-launcher and
// React Native's bundle/HMR runtime, primarily).
//
// Flow:
//
//  1. Owner calls POST /v1/preview/tokens/{token}/sign (JWT-auth'd)
//     → gateway returns a signed URL of the form
//     `https://preview-<token>.<root>/?clank_sig=<sig>&clank_exp=<unix>`.
//
//  2. Owner hands that URL to PreviewLauncher.launchPreview(url).
//
//  3. Gateway's preview proxy, on receiving the first request, sees
//     `clank_sig` + `clank_exp` in the query, verifies the HMAC,
//     and sets cookies (clank_sig + clank_exp) scoped to the
//     preview host so subsequent runtime fetches carry the bearer
//     automatically — they don't need to know about the signature.
//
//  4. Subsequent fetches present the cookie; gateway treats it as
//     equivalent to the query bearer.
//
// Security model:
//
//   - HMAC-SHA-256 over `<token>|<exp_unix>` with a server-side
//     secret (Config.PreviewSigningKey). 32 bytes of entropy is the
//     floor; random-on-startup is the fallback when not configured.
//   - Signature is bound to the specific token, so leaking a sig
//     for token A doesn't grant access to token B.
//   - exp is in the signed payload, so an attacker can't extend
//     the TTL by tampering with the URL.
//   - The sig is unguessable without the secret, so 128-bit
//     token-unguessability + 256-bit sig-unguessability stack.
//
// The proxy still falls back to JWT auth on the inbound request, so
// owner-only access via clients that DO carry a JWT (the mobile API
// caller, curl, etc.) keeps working without signing.

// SigParam and ExpParam are the reserved query/cookie keys.
// "clank_" prefix keeps them from colliding with anything the dev
// server might use.
const (
	SigParam = "clank_sig"
	ExpParam = "clank_exp"
)

// DefaultSigTTL is the default lifetime of a signed URL. Short on
// purpose — a freshly-launched preview signs at mount time and the
// dev-launcher only needs the bearer until the bundle has loaded +
// HMR is established (~seconds in practice).
const DefaultSigTTL = 15 * time.Minute

// MaxSigTTL caps how long callers can request via the share/sign
// endpoint. 24h matches DefaultTokenTTL — there's no reason for a
// signature to outlive the token row that backs it.
const MaxSigTTL = 24 * time.Hour

// MinSigningKeyBytes is the floor for Config.PreviewSigningKey.
// 32 bytes = 256 bits = the SHA-256 output size, which is the
// security parameter HMAC-SHA-256 caps out at.
const MinSigningKeyBytes = 32

// ErrInvalidSignature is returned by Verify when the sig doesn't
// match. Callers shouldn't differentiate from "missing" — both are
// "fail closed."
var ErrInvalidSignature = errors.New("preview: invalid signature")

// ErrSignatureExpired is returned by Verify when exp is in the past.
// Separated from ErrInvalidSignature so log lines can distinguish
// "stale share link" from "tampered URL."
var ErrSignatureExpired = errors.New("preview: signature expired")

// GenerateSigningKey returns a fresh random key suitable for
// Config.PreviewSigningKey. Used by gateways that don't have a
// persisted secret configured.
func GenerateSigningKey() ([]byte, error) {
	key := make([]byte, MinSigningKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("preview: generate signing key: %w", err)
	}
	return key, nil
}

// Sign produces the HMAC-SHA-256 of `<token>|<exp_unix>` keyed by
// secret, base64url-encoded with no padding. The caller is
// responsible for choosing the exp (typically now() + TTL).
func Sign(secret []byte, token string, exp time.Time) (string, error) {
	if len(secret) < MinSigningKeyBytes {
		return "", fmt.Errorf("preview: signing key shorter than %d bytes", MinSigningKeyBytes)
	}
	if token == "" {
		return "", errors.New("preview: token is required")
	}
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s|%d", token, exp.Unix())
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify returns nil iff sig matches Sign(secret, token, exp) AND
// exp is in the future. Use VerifyFromRequest from handler code so
// the query/cookie extraction stays in one place.
func Verify(secret []byte, token, sig string, exp time.Time, now time.Time) error {
	if !exp.After(now) {
		return ErrSignatureExpired
	}
	expected, err := Sign(secret, token, exp)
	if err != nil {
		return err
	}
	// Constant-time compare to avoid timing leaks on the prefix.
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return ErrInvalidSignature
	}
	return nil
}

// VerifyFromRequest extracts (sig, exp) from the request's query
// or cookies (query takes precedence — it's the first-touch path)
// and validates against token. Returns nil on a valid signature,
// ErrInvalidSignature when no signature is present OR the signature
// fails, ErrSignatureExpired when the exp is past.
//
// The "no signature present" case maps to ErrInvalidSignature
// because at the handler layer it has the same meaning: fail
// closed. The handler can choose to translate to 401/404/etc.
func VerifyFromRequest(secret []byte, token string, r *http.Request, now time.Time) error {
	sig, exp, ok := extractSignedParams(r)
	if !ok {
		return ErrInvalidSignature
	}
	return Verify(secret, token, sig, exp, now)
}

// extractSignedParams pulls sig + exp from the request. Query
// parameters win when present (signed-URL first-touch); otherwise
// fall back to cookies (subsequent runtime fetches after the proxy
// set them on the first response).
func extractSignedParams(r *http.Request) (sig string, exp time.Time, ok bool) {
	q := r.URL.Query()
	if s := q.Get(SigParam); s != "" {
		if e := q.Get(ExpParam); e != "" {
			t, err := parseUnix(e)
			if err == nil {
				return s, t, true
			}
		}
	}
	if c, err := r.Cookie(SigParam); err == nil && c.Value != "" {
		if ec, err := r.Cookie(ExpParam); err == nil && ec.Value != "" {
			t, err := parseUnix(ec.Value)
			if err == nil {
				return c.Value, t, true
			}
		}
	}
	return "", time.Time{}, false
}

func parseUnix(s string) (time.Time, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(n, 0), nil
}

// SignedURL returns a fully-formed URL for token with sig + exp
// query parameters set.
//
// scheme + port come from the inbound API request (typically
// derived via SchemeFromRequest + PortFromHost), so the URL the
// client follows matches the same origin it's already talking to.
// Pass scheme="https" + port="" for the standard cloud deploy;
// scheme="http" + port="7878" for local docker dev.
func SignedURL(secret []byte, token, rootDomain, scheme, port string, exp time.Time) (string, error) {
	sig, err := Sign(secret, token, exp)
	if err != nil {
		return "", err
	}
	if scheme == "" {
		scheme = "https"
	}
	host := HostFor(token, rootDomain)
	if port != "" {
		host = host + ":" + port
	}
	u := url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   "/",
	}
	q := u.Query()
	q.Set(SigParam, sig)
	q.Set(ExpParam, strconv.FormatInt(exp.Unix(), 10))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// SchemeFromRequest returns "https" when the request reached us over
// TLS (directly via r.TLS or via an upstream TLS terminator advertised
// in X-Forwarded-Proto), otherwise "http". Used by handlers that need
// to mint an absolute URL the client will be able to follow back.
func SchemeFromRequest(r *http.Request) string {
	if RequestIsHTTPS(r) {
		return "https"
	}
	return "http"
}

// PortFromHost extracts the :port from an HTTP Host header, or "" if
// the host has no explicit port (i.e. it's served on the protocol
// default — 80 for http, 443 for https). Used to thread the API
// listen port back into the preview host so http://localhost:7878
// stays reachable.
func PortFromHost(host string) string {
	// Strip any IPv6 brackets first.
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		// IPv6 with port looks like "[::1]:7878" — only treat the
		// final colon as the port separator when the segment that
		// follows is numeric.
		port := host[i+1:]
		if port == "" {
			return ""
		}
		for _, c := range port {
			if c < '0' || c > '9' {
				return ""
			}
		}
		return port
	}
	return ""
}

// SetSignedCookies writes Set-Cookie headers carrying sig + exp on
// w. Called by the gateway proxy on the first valid signed-URL
// request so subsequent runtime fetches don't need to re-carry the
// query params. Both cookies are scoped to the path "/" and the
// host that served the request (omitting Domain pins them to the
// preview hostname, not the entire root zone).
//
// The cookies expire at exp; clients drop them automatically.
//
// `secure` controls the Secure flag. Pass true when the request
// reached us over TLS (typical production); pass false for plain
// HTTP local dev (browsers drop Secure cookies on http://). The
// caller computes this from r.TLS != nil OR X-Forwarded-Proto
// (set by an upstream TLS terminator).
//
// Browsers honor HttpOnly + SameSite=Strict; React Native's
// NSURLSession / OkHttp cookie jars do too (with the caveat that
// Expo dev-launcher's HTTP layer is the empirical question we'd
// need to verify per platform).
func SetSignedCookies(w http.ResponseWriter, sig string, exp time.Time, secure bool) {
	maxAge := int(time.Until(exp).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SigParam,
		Value:    sig,
		Path:     "/",
		MaxAge:   maxAge,
		Expires:  exp,
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     ExpParam,
		Value:    strconv.FormatInt(exp.Unix(), 10),
		Path:     "/",
		MaxAge:   maxAge,
		Expires:  exp,
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// RequestIsHTTPS reports whether the incoming request reached the
// gateway over TLS, either directly (r.TLS != nil) or via an upstream
// terminator that set X-Forwarded-Proto. Used by handlers that need
// to decide the Secure flag on outgoing cookies.
func RequestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}
