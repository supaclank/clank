package daytona

import (
	"net/http"
)

// previewTokenInjector adds Daytona's `x-daytona-preview-token`
// header to every outbound request before delegating to wrapped.
type previewTokenInjector struct {
	wrapped http.RoundTripper
	token   string
}

// RoundTrip implements http.RoundTripper.
func (p *previewTokenInjector) RoundTrip(r *http.Request) (*http.Response, error) {
	// RoundTrip must not modify its input — clone before setting headers.
	r2 := r.Clone(r.Context())
	r2.Header = r.Header.Clone()
	r2.Header.Set("x-daytona-preview-token", p.token)
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
