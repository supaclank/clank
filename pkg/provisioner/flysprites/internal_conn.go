// Sprites-side implementation of the GetHostByID + OpenInternalConn
// capability extensions on provisioner.Provisioner.
//
// GetHostByID is a non-mutating store lookup that builds the same
// HostRef shape EnsureHost returns — but without provisioning, waking
// or installing. Used by the preview-route proxy to resolve a token's
// target host before tunneling. (The Sprites edge wakes on traffic to
// the public URL, so OpenInternalConn's WSS to api.sprites.dev is
// what actually causes a hibernated sprite to wake on the first
// preview request; GetHostByID itself doesn't touch sprite state.)
//
// OpenInternalConn wraps sprites-go's ProxySocket: a transparent TCP
// relay tunneled over a fresh WSS to api.sprites.dev/v1/sprites/<name>
// /proxy, authenticated with the org's SPRITES_TOKEN that the
// Provisioner already holds. The same primitive the Phase 0 spike
// validated.
package flysprites

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"

	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
	transportpkg "github.com/acksell/clank/pkg/provisioner/transport"
)

// GetHostByID looks up a sprite by stored host_id and builds a
// HostRef pointing at it. Mirrors the shape EnsureHost returns but
// skips provisioning, waking and installation — strictly a read.
//
// Errors:
//   - hoststore.ErrHostNotFound (wrapped) when the row doesn't exist
//   - any other store error wrapped with context
//   - a bad LastURL in the row is treated as a corrupt row and
//     surfaced as a hard error (we can't construct a transport
//     without parsing the host out)
func (p *Provisioner) GetHostByID(ctx context.Context, hostID string) (provisioner.HostRef, error) {
	if hostID == "" {
		return provisioner.HostRef{}, fmt.Errorf("flyio provisioner: hostID is required")
	}
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		if errors.Is(err, hoststore.ErrHostNotFound) {
			return provisioner.HostRef{}, err
		}
		return provisioner.HostRef{}, fmt.Errorf("flyio provisioner: store get-by-id: %w", err)
	}
	if row.LastURL == "" {
		return provisioner.HostRef{}, fmt.Errorf("flyio provisioner: host %s has no last_url (corrupt row)", hostID)
	}
	parsedURL, err := url.Parse(row.LastURL)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("flyio provisioner: parse host URL %q: %w", row.LastURL, err)
	}
	return provisioner.HostRef{
		HostID:    row.ID,
		URL:       row.LastURL,
		Transport: &transportpkg.BearerInjector{Token: row.AuthToken, Host: parsedURL.Host},
		AuthToken: row.AuthToken,
		AutoWake:  true, // Sprites edge wakes on traffic
		Hostname:  row.Hostname,
	}, nil
}

// OpenInternalConn opens a transparent TCP tunnel from the gateway
// to (port) inside the sprite identified by hostID. Uses sprites-go's
// ProxySocket — the same primitive Phase 0 verified handles
// HTTP/1.1, WebSocket upgrade, and concurrent reuse cleanly.
//
// The returned net.Conn is single-shot: closing it closes the
// underlying WSS. The caller (typically previewtunnel.RoundTripper)
// is expected to pool conns at the http.Transport layer.
//
// host inside the sprite is always "localhost" — the spike confirmed
// Sprites' proxy resolves it to the sprite's loopback iface, where
// per-worktree dev servers (Metro etc.) actually bind.
func (p *Provisioner) OpenInternalConn(ctx context.Context, hostID string, port int) (net.Conn, error) {
	if hostID == "" {
		return nil, fmt.Errorf("flyio provisioner: hostID is required")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("flyio provisioner: port %d out of range", port)
	}
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		if errors.Is(err, hoststore.ErrHostNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("flyio provisioner: store get-by-id: %w", err)
	}
	if row.ExternalID == "" {
		return nil, fmt.Errorf("flyio provisioner: host %s has no external_id (sprite name unknown)", hostID)
	}
	conn, err := p.client.ProxySocket(ctx, "tcp", row.ExternalID, fmt.Sprintf("localhost:%d", port))
	if err != nil {
		return nil, fmt.Errorf("flyio provisioner: proxy socket to %s:%d: %w", row.ExternalID, port, err)
	}
	return conn, nil
}
