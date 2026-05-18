package gateway

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/acksell/clank/pkg/provisioner/transport"
)

// newRemoteReverseProxy builds an httputil.ReverseProxy that forwards
// requests to baseURL with the active remote profile's bearer token
// injected by transport.BearerInjector.
//
// SSE-safe: FlushInterval = -1 means events are written to the client
// as they arrive, with no proxy-side buffering. The inbound and
// outbound connections share their lifetime — when the laptop CLI
// disconnects, the daemon's outbound SSE drops and the sprite's
// last-consumer timer starts. That's the auto-sleep property we care
// about preserving.
//
// The inbound Authorization header is stripped before forwarding so
// the laptop daemon's local auth.AllowAll bearer doesn't leak to the
// remote and the BearerInjector's per-host token takes over.
func newRemoteReverseProxy(baseURL, jwt string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: parse remote URL %q: %w", baseURL, err)
	}
	if target.Host == "" {
		return nil, fmt.Errorf("gateway: remote URL %q is missing host", baseURL)
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			// Strip the inbound Authorization so the laptop's local
			// auth.AllowAll bearer doesn't reach the remote; the
			// BearerInjector below sets the correct one.
			pr.Out.Header.Del("Authorization")
		},
		Transport: &transport.BearerInjector{
			Wrapped: http.DefaultTransport,
			Token:   jwt,
			Host:    target.Host,
		},
		FlushInterval: -1,
	}, nil
}
