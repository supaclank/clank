package hostmux

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireBearer rejects HTTP requests missing a matching
// Authorization: Bearer <token> header. When token == "", the
// middleware is a no-op so non-cloud paths (laptop-local subprocess
// launcher, in-process tests) keep working unchanged.
//
// The token compare is constant-time to avoid leaking the secret
// through timing differences across mismatched prefixes — a real
// concern when a public sprite URL is reachable from anywhere on
// the internet.
func requireBearer(token string) func(http.Handler) http.Handler {
	if token == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	want := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := bearerFromHeader(r.Header.Get("Authorization"))
			if got == "" || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="clank-host"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerFromHeader extracts the token from a "Bearer <token>"
// Authorization header. Returns "" if the header is missing or
// malformed; the caller treats either as a 401.
func bearerFromHeader(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
