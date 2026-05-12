package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// OIDCConfig configures an OIDC Authenticator. Issuer + Audience are
// required; JWKSURL is optional (discovered via the issuer's
// .well-known/openid-configuration when unset).
type OIDCConfig struct {
	// Issuer is the OIDC provider's issuer URL. Required. Enforced
	// against the JWT "iss" claim and (when JWKSURL is unset) used
	// for discovery of jwks_uri.
	Issuer string

	// Audience is the expected JWT "aud" claim. Required. clankd
	// deployments typically set this to a clankd-specific audience
	// configured at the IdP (e.g. "clank-api").
	Audience string

	// JWKSURL is the JWK Set endpoint. Optional. When unset,
	// resolved via OIDC discovery from
	// {Issuer}/.well-known/openid-configuration.
	JWKSURL string

	// UserClaim is the claim name used to populate Principal.UserID.
	// Optional; defaults to "sub".
	UserClaim string

	// Algorithms restricts the accepted signing algorithms. Optional;
	// defaults to ["RS256", "ES256"]. "none" is never accepted.
	Algorithms []string

	// HTTPTimeout caps the OIDC discovery HTTP request. Optional;
	// defaults to 10s. JWKS fetches and refreshes use the keyfunc
	// library's defaults — configure those separately if needed.
	HTTPTimeout time.Duration
}

// OIDC verifies RS256/ES256 JWTs against a JWKS-published key set,
// enforces iss + aud claims, and maps a configurable claim to the
// Principal's UserID. Works with any standard OIDC provider (Auth0,
// Okta, Keycloak, Microsoft Entra, Google Workspace, Supabase, ...).
type OIDC struct {
	cfg        OIDCConfig
	algorithms []string
	kf         keyfunc.Keyfunc
}

// NewOIDC constructs an OIDC Authenticator. Performs the initial
// JWKS fetch (and, when JWKSURL is unset, OIDC discovery). Returns
// an error when the IdP is unreachable, which surfaces
// misconfiguration at startup rather than at first request.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("auth: OIDCConfig.Issuer is required")
	}
	if cfg.Audience == "" {
		return nil, fmt.Errorf("auth: OIDCConfig.Audience is required")
	}
	timeout := cfg.HTTPTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		discovered, err := discoverJWKSURL(ctx, cfg.Issuer, timeout)
		if err != nil {
			return nil, fmt.Errorf("auth: OIDC discovery: %w", err)
		}
		jwksURL = discovered
	}

	kf, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("auth: OIDC JWKS init (%s): %w", jwksURL, err)
	}

	algs := cfg.Algorithms
	if len(algs) == 0 {
		algs = []string{jwt.SigningMethodRS256.Alg(), jwt.SigningMethodES256.Alg()}
	}

	return &OIDC{cfg: cfg, algorithms: algs, kf: kf}, nil
}

// Verify validates a bearer JWT against the provider's JWKS and the
// configured issuer/audience/algorithms, then maps UserClaim to the
// Principal.
func (a *OIDC) Verify(r *http.Request) (Principal, error) {
	tok, err := ExtractBearer(r)
	if err != nil {
		return Principal{}, err
	}

	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(tok, claims, a.kf.KeyfuncCtx(r.Context()),
		jwt.WithValidMethods(a.algorithms),
		jwt.WithIssuer(a.cfg.Issuer),
		jwt.WithAudience(a.cfg.Audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}

	claimName := a.cfg.UserClaim
	if claimName == "" {
		claimName = "sub"
	}
	v, _ := claims[claimName].(string)
	if v == "" {
		return Principal{}, fmt.Errorf("%w: missing or non-string %q claim", ErrUnauthenticated, claimName)
	}
	return Principal{UserID: v, Claims: map[string]any(claims)}, nil
}

// discoverJWKSURL fetches {issuer}/.well-known/openid-configuration
// and returns the jwks_uri field. Standard RFC 8414 / OIDC discovery.
func discoverJWKSURL(ctx context.Context, issuer string, timeout time.Duration) (string, error) {
	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery %s: HTTP %d", url, resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("discovery %s: decode: %w", url, err)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("discovery %s: missing jwks_uri", url)
	}
	return doc.JWKSURI, nil
}
