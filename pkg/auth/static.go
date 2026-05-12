package auth

import (
	"crypto/subtle"
	"fmt"
	"net/http"
)

// StaticBearer verifies a fixed shared-secret Bearer token via
// constant-time compare. On match, every request resolves to the
// configured UserID. Useful only for single-user / self-hosted
// fallback (CLANK_AUTH_TOKEN); production deployments should use
// JWTHS256 or OIDC.
type StaticBearer struct {
	// Token is the expected bearer. Required.
	Token string

	// UserID is the Principal.UserID populated on a successful match.
	// Required (no implicit default — callers must decide what user
	// the shared bearer represents, e.g. the OS username).
	UserID string
}

// Verify compares the bearer to the configured token in constant
// time and returns Principal{UserID: a.UserID} on match.
func (a *StaticBearer) Verify(r *http.Request) (Principal, error) {
	if a.Token == "" {
		return Principal{}, fmt.Errorf("auth: StaticBearer.Token is empty")
	}
	if a.UserID == "" {
		return Principal{}, fmt.Errorf("auth: StaticBearer.UserID is empty")
	}
	tok, err := ExtractBearer(r)
	if err != nil {
		return Principal{}, err
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(a.Token)) != 1 {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{UserID: a.UserID}, nil
}
