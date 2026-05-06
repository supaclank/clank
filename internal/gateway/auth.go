package gateway

import "net/http"

// Authenticator verifies the bearer on an incoming request and
// returns claims (e.g. JWT claims) for downstream lookups. The
// claims map is intentionally untyped — PR 4's real JWT verifier
// fills in {sub, iss, exp, ...}; the PR 3 stub returns just
// {sub: "local"}.
//
// An error from Verify causes the gateway to return 401.
type Authenticator interface {
	Verify(r *http.Request) (claims map[string]any, err error)
}

// PermissiveAuth accepts every request — used in PR 3 because the
// daemon is single-user and the JWT issuance flow doesn't exist
// yet. PR 4 replaces this with a real JWKS-backed verifier without
// changing any other part of the gateway.
type PermissiveAuth struct{}

// Verify always succeeds and returns a stub claim set.
func (PermissiveAuth) Verify(*http.Request) (map[string]any, error) {
	return map[string]any{"sub": "local"}, nil
}
