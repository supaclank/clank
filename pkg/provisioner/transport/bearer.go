// Package transport holds RoundTripper helpers shared across
// provisioners. The provisioner-specific transports were near-
// identical copies until three duplicates accumulated; this is the
// "third copy → extract" trigger.
package transport

import "net/http"

// BearerInjector adds `Authorization: Bearer <Token>` to outbound
// requests. The Host field pins the injection to a single upstream:
// when non-empty, the bearer is only set when r.URL.Host == Host.
// This prevents the token from leaking when the upstream issues a
// cross-host redirect and the client (or a wrapping http.Client)
// follows it via the same Transport.
//
// Empty Host means "inject everywhere" — used only by callers that
// haven't pinned yet (e.g. test fixtures); production constructors
// always pass the parsed upstream host.
type BearerInjector struct {
	Wrapped http.RoundTripper
	Token   string
	Host    string
}

// RoundTrip implements http.RoundTripper.
func (b *BearerInjector) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header = r.Header.Clone()
	if b.Token != "" && (b.Host == "" || r2.URL.Host == b.Host) {
		r2.Header.Set("Authorization", "Bearer "+b.Token)
	}
	rt := b.Wrapped
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(r2)
}

// CloseIdleConnections delegates so wrapping us doesn't hide the pool.
func (b *BearerInjector) CloseIdleConnections() {
	type idler interface{ CloseIdleConnections() }
	rt := b.Wrapped
	if rt == nil {
		rt = http.DefaultTransport
	}
	if i, ok := rt.(idler); ok {
		i.CloseIdleConnections()
	}
}
