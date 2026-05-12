// Package auth is the single contract for authenticating inbound
// HTTP requests in clank. One Authenticator interface, one Principal
// type, one Middleware. Self-hosters and embedders plug in custom
// verifiers (Supabase, Auth0, Keycloak, custom DB lookup, etc.) by
// implementing Authenticator and passing it via daemoncli.ServerOptions.Auth.
//
// Bundled implementations:
//
//   - JWTHS256: HS256 JWT verifier (dev / shared-secret deployments).
//   - OIDC:     RS256/ES256 JWT + JWKS verifier (production / SSO).
//   - StaticBearer: fixed shared secret (opt-in CLANK_AUTH_TOKEN path).
//   - AllowAll: no-op verifier for the unix-socket listener and tests.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Principal is the verified caller identity. Middleware injects it
// into the request context; downstream handlers read it via
// MustPrincipal. Claims carries the raw claim map produced by the
// underlying verifier (JWT payload, OAuth introspection response,
// etc.); it may be nil for verifiers that don't have one (e.g.
// StaticBearer, AllowAll).
type Principal struct {
	UserID string
	Claims map[string]any
}

// Authenticator verifies an inbound request and returns the caller's
// Principal. Implementations should return ErrUnauthenticated (or a
// wrapped version) so Middleware can map the failure to 401.
type Authenticator interface {
	Verify(r *http.Request) (Principal, error)
}

// ErrUnauthenticated is the sentinel for "no/invalid credentials".
// Authenticator implementations should return this (or wrap it) and
// Middleware maps it to HTTP 401.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

type ctxKey struct{}

// WithPrincipal returns a copy of ctx with p stored. Middleware sets
// it after a successful Verify; downstream code reads via
// PrincipalFrom or MustPrincipal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the Principal stored in ctx, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// MustPrincipal returns the Principal stored in ctx. Panics if absent —
// use at handler boundaries that are guaranteed to run after
// Middleware. Surfaces middleware-misconfiguration bugs loudly
// instead of silently producing requests with an empty UserID.
func MustPrincipal(ctx context.Context) Principal {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		panic("auth: no Principal in context — middleware not wired?")
	}
	return p
}

// BearerPrefix is the case-sensitive prefix of the Authorization
// header value used throughout. Exported so verifiers don't have to
// agree on a magic string.
const BearerPrefix = "Bearer "

// ExtractBearer returns the token portion of the Authorization
// header, or empty + ErrUnauthenticated when absent/malformed. Helper
// for Authenticator implementations.
func ExtractBearer(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, BearerPrefix) {
		return "", ErrUnauthenticated
	}
	tok := strings.TrimSpace(h[len(BearerPrefix):])
	if tok == "" {
		return "", ErrUnauthenticated
	}
	return tok, nil
}

// Middleware runs a.Verify on every request and, on success, injects
// the resulting Principal into the request context before delegating
// to next. On failure it returns 401 with a WWW-Authenticate header.
func Middleware(next http.Handler, a Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Verify(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="clank"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := WithPrincipal(r.Context(), p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
