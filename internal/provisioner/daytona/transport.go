package daytona

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"

	transportpkg "github.com/acksell/clank/internal/provisioner/transport"
)

// previewTokenInjector adds Daytona's `x-daytona-preview-token`
// header. host pins the injection to the upstream so a cross-host
// redirect can't carry the token to a third-party.
type previewTokenInjector struct {
	wrapped http.RoundTripper
	token   string
	host    string
}

// RoundTrip implements http.RoundTripper.
func (p *previewTokenInjector) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header = r.Header.Clone()
	if p.token != "" && (p.host == "" || r2.URL.Host == p.host) {
		r2.Header.Set("x-daytona-preview-token", p.token)
	}
	rt := p.wrapped
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(r2)
}

// CloseIdleConnections delegates so wrapping us doesn't hide the pool.
func (p *previewTokenInjector) CloseIdleConnections() {
	type idler interface{ CloseIdleConnections() }
	rt := p.wrapped
	if rt == nil {
		rt = http.DefaultTransport
	}
	if i, ok := rt.(idler); ok {
		i.CloseIdleConnections()
	}
}

// chainTransport stacks the bearer (auth-token, app layer) on top of
// the preview-token (Daytona edge layer). previewURL pins both to the
// upstream's host so cross-host redirects can't leak either credential.
// authToken is required — chainTransport is only called on paths
// where the store row gave us a non-empty value.
func chainTransport(authToken, previewToken, previewURL string) (http.RoundTripper, error) {
	parsed, err := url.Parse(previewURL)
	if err != nil {
		return nil, fmt.Errorf("parse preview URL %q: %w", previewURL, err)
	}
	rt := http.RoundTripper(&previewTokenInjector{token: previewToken, host: parsed.Host})
	if authToken != "" {
		rt = &transportpkg.BearerInjector{Wrapped: rt, Token: authToken, Host: parsed.Host}
	}
	return rt, nil
}

// generateAuthToken returns a cryptographically-random URL-safe token
// suitable for the require-bearer middleware. ~256 bits of entropy.
func generateAuthToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
