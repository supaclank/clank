// Package tunnelclient dials clank-host's GET /tunnel/{port}
// endpoint and adapts the WebSocket to a net.Conn carrying the raw
// TCP bytes of a port on the host's loopback.
//
// Provisioners use it to implement OpenInternalConn on providers
// whose edge only routes declared service ports (e.g. Fly Machines
// behind a Flycast address), where a provider-side TCP-proxy
// primitive doesn't exist. The server side is internal/host/mux's
// tunnel handler; auth rides the same chain as every other host
// request (HostRef.Transport).
package tunnelclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/coder/websocket"
)

// ErrPortUnreachable reports that clank-host answered but could not
// dial the requested loopback port — the dev server isn't listening
// (yet). Gateways map this to a 502 toward the client rather than
// treating the host as unreachable.
var ErrPortUnreachable = errors.New("tunnelclient: target port unreachable on host")

// Dial opens a TCP-over-WebSocket tunnel to (port) on the loopback of
// the host at baseURL. rt carries the full auth chain to reach the
// host (typically HostRef.Transport). ctx bounds the dial only; the
// returned conn lives until Close — required because callers like
// previewtunnel cancel their dial context the moment the dial
// returns.
func Dial(ctx context.Context, baseURL string, rt http.RoundTripper, port int) (net.Conn, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("tunnelclient: baseURL is required")
	}
	if rt == nil {
		return nil, fmt.Errorf("tunnelclient: transport is required")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("tunnelclient: port %d out of range", port)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("tunnelclient: parse base URL %q: %w", baseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("tunnelclient: baseURL %q must be an absolute URL with scheme and host", baseURL)
	}
	tunnelURL := u.JoinPath("tunnel", strconv.Itoa(port))

	// websocket.Dial accepts http(s) URLs and switches to ws(s) itself.
	c, resp, err := websocket.Dial(ctx, tunnelURL.String(), &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: rt},
	})
	if err != nil {
		// TODO(ai-review): match on hostmux's JSON "port_unreachable" code, not bare 502, so an intermediary's unrelated 502 isn't misclassified. https://github.com/Acksell/clank/pull/125
		if resp != nil && resp.StatusCode == http.StatusBadGateway {
			return nil, fmt.Errorf("tunnelclient: dial port %d on %s: %w", port, u.Host, ErrPortUnreachable)
		}
		return nil, fmt.Errorf("tunnelclient: websocket dial %s: %w", tunnelURL, err)
	}
	// Proxied streams are opaque and unbounded; mirror the server side.
	c.SetReadLimit(-1)

	return websocket.NetConn(context.WithoutCancel(ctx), c, websocket.MessageBinary), nil
}
