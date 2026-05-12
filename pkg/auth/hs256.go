package auth

import (
	"fmt"
	"net/http"

	"github.com/golang-jwt/jwt/v5"
)

// JWTHS256 verifies HS256 JWTs signed with a shared Secret. Used by
// dev profiles (clank-auth-stub) and self-hosted single-secret
// deployments. For production OIDC/SSO, use OIDC instead.
type JWTHS256 struct {
	// Secret is the HMAC key. Required.
	Secret []byte

	// ClaimMapper extracts a Principal from verified claims. When nil,
	// the default mapper uses claims["sub"] as the UserID and stores
	// the full claim map on Principal.Claims.
	ClaimMapper func(jwt.MapClaims) (Principal, error)
}

// Verify parses and verifies a Bearer token, then maps claims to a
// Principal. Uses jwt/v5's WithValidMethods to reject any algorithm
// other than HS256 (algorithm-confusion guard) and its built-in
// exp/nbf/iat validation.
func (a *JWTHS256) Verify(r *http.Request) (Principal, error) {
	if len(a.Secret) == 0 {
		return Principal{}, fmt.Errorf("auth: JWTHS256.Secret is empty")
	}
	tok, err := ExtractBearer(r)
	if err != nil {
		return Principal{}, err
	}

	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(tok, claims, func(*jwt.Token) (any, error) {
		return a.Secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}

	mapper := a.ClaimMapper
	if mapper == nil {
		mapper = defaultSubMapper
	}
	return mapper(claims)
}

// defaultSubMapper returns Principal{UserID: claims["sub"]} and
// stashes the full claim map. Shared by JWTHS256 and OIDC.
func defaultSubMapper(c jwt.MapClaims) (Principal, error) {
	sub, _ := c["sub"].(string)
	if sub == "" {
		return Principal{}, fmt.Errorf("%w: missing or non-string sub claim", ErrUnauthenticated)
	}
	return Principal{UserID: sub, Claims: map[string]any(c)}, nil
}
